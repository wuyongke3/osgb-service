package main

// util.go holds small shared helpers: identifier generation, path validation,
// input inspection and command-line tokenisation.

import (
	"crypto/rand"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync/atomic"
	"time"
)

var idCounter atomic.Uint64

// newID returns a unique, filesystem-safe identifier of the form
// <unix-nano>-<counter><random>. The counter covers same-nanosecond calls and
// the random suffix from crypto/rand keeps IDs unguessable across restarts.
func newID() string {
	seq := idCounter.Add(1)
	return fmt.Sprintf("%d-%s%s", time.Now().UnixNano(), encodeBase36(seq), randomToken(6))
}

// encodeBase36 renders a number in base 36 so the counter stays short in paths.
func encodeBase36(value uint64) string {
	const alphabet = "0123456789abcdefghijklmnopqrstuvwxyz"
	if value == 0 {
		return "0"
	}
	var buf [13]byte
	i := len(buf)
	for value > 0 {
		i--
		buf[i] = alphabet[value%36]
		value /= 36
	}
	return string(buf[i:])
}

// randomToken returns a cryptographically random token of the requested length.
// It falls back to a time-seeded generator only if the system entropy source is
// unavailable, which would otherwise make every caller fail.
func randomToken(length int) string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	if length <= 0 {
		return ""
	}
	buf := make([]byte, length)
	if _, err := rand.Read(buf); err != nil {
		seed := uint64(time.Now().UnixNano())
		for i := range buf {
			seed = seed*6364136223846793005 + 1442695040888963407
			buf[i] = byte(seed >> 33)
		}
	}
	var value strings.Builder
	value.Grow(length)
	for _, b := range buf {
		value.WriteByte(alphabet[int(b)%len(alphabet)])
	}
	return value.String()
}

func findModel(projectDir string) (string, error) {
	preferred := []string{
		filepath.Join(projectDir, "odm_texturing", "odm_textured_model_geo.obj"),
		filepath.Join(projectDir, "odm_texturing", "odm_textured_model.obj"),
		filepath.Join(projectDir, "odm_25dtexturing", "odm_textured_model_geo.obj"),
		filepath.Join(projectDir, "odm_25dtexturing", "odm_textured_model.obj"),
	}
	for _, path := range preferred {
		if info, err := os.Stat(path); err == nil && !info.IsDir() {
			return filepath.Abs(path)
		}
	}
	var candidates []string
	err := filepath.WalkDir(projectDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		ext := strings.ToLower(filepath.Ext(path))
		if ext == ".obj" || ext == ".ply" || ext == ".glb" {
			candidates = append(candidates, path)
		}
		return nil
	})
	if err != nil {
		return "", fmt.Errorf("search reconstructed model: %w", err)
	}
	sort.Strings(candidates)
	if len(candidates) == 0 {
		return "", errors.New("reconstruction finished but no OBJ/PLY/GLB model was found")
	}
	return filepath.Abs(candidates[0])
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func inspectInputPath(inputPath string) (int, int64, error) {
	if inputPath == "" {
		return 0, 0, errors.New("input_path is required")
	}
	if !filepath.IsAbs(inputPath) {
		return 0, 0, errors.New("input_path must be an absolute local directory path")
	}
	info, err := os.Stat(inputPath)
	if err != nil {
		return 0, 0, fmt.Errorf("input path is not accessible: %w", err)
	}
	if !info.IsDir() {
		return 0, 0, errors.New("input_path must be a directory containing flight images")
	}
	scanRoot := inputPath
	if imagesInfo, imagesErr := os.Stat(filepath.Join(inputPath, "images")); imagesErr == nil && imagesInfo.IsDir() {
		scanRoot = filepath.Join(inputPath, "images")
	}
	count := 0
	var bytes int64
	err = filepath.WalkDir(scanRoot, func(_ string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() {
			return nil
		}
		if !isSupportedImage(entry.Name()) {
			return nil
		}
		fileInfo, statErr := entry.Info()
		if statErr != nil {
			return statErr
		}
		count++
		bytes += fileInfo.Size()
		return nil
	})
	if err != nil {
		return 0, 0, fmt.Errorf("scan input images: %w", err)
	}
	if count == 0 {
		return 0, 0, errors.New("no supported images found; use JPG, PNG, TIFF or WebP files")
	}
	return count, bytes, nil
}

func pickWindowsPath(kind string) (string, error) {
	// The dialog strings are deliberately ASCII-only: the script is passed to
	// PowerShell through -Command, and non-ASCII text here has previously been
	// corrupted into mojibake by console/GBK code-page translation.
	script := `$ErrorActionPreference = 'Stop'; Add-Type -AssemblyName System.Windows.Forms; `
	if kind == "output" {
		script += `$dialog = New-Object System.Windows.Forms.SaveFileDialog; $dialog.Title = 'Choose the OSGB output file (for example root.osgb)'; $dialog.Filter = 'OSGB files (*.osgb)|*.osgb|All files (*.*)|*.*'; $dialog.DefaultExt = 'osgb'; if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::WriteLine($dialog.FileName) }`
	} else {
		script += `$dialog = New-Object System.Windows.Forms.FolderBrowserDialog; $dialog.Description = 'Choose the folder containing the flight images for reconstruction'; $dialog.UseDescriptionForTitle = $true; if ($dialog.ShowDialog() -eq [System.Windows.Forms.DialogResult]::OK) { [Console]::WriteLine($dialog.SelectedPath) }`
	}
	for _, binary := range []string{"powershell.exe", "pwsh.exe"} {
		if _, err := exec.LookPath(binary); err != nil {
			continue
		}
		command := exec.Command(binary, "-NoProfile", "-STA", "-Command", script)
		output, err := command.Output()
		if err != nil {
			return "", fmt.Errorf("open native folder picker: %w", err)
		}
		return strings.TrimSpace(string(output)), nil
	}
	return "", errors.New("PowerShell is not available")
}

func expandArgs(template string, vars map[string]string) ([]string, error) {
	tokens, err := tokenize(template)
	if err != nil {
		return nil, err
	}
	for i := range tokens {
		for key, value := range vars {
			tokens[i] = strings.ReplaceAll(tokens[i], key, value)
		}
	}
	return tokens, nil
}

// tokenize splits a command template into argv, honoring single and double
// quotes.
//
// Backslash handling is deliberately conservative because these templates carry
// Windows paths. Historically a backslash was always treated as an escape, which
// silently mangled any Windows path: `-o "C:\data\job\out.mvs"` produced the
// single token `C:datajobout.mvs` and the tool then wrote to a nonsense path.
// Forward-slash paths happened to work, so the defect went unnoticed while the
// shipped defaults all used "/".
//
// Rules now applied:
//   - Inside single quotes, everything is literal (no escapes at all).
//   - A backslash only escapes a character when that character would otherwise
//     be meaningful here, i.e. a quote, a backslash, or whitespace. This keeps
//     `\"` and `\ ` working as escapes while `C:\data` and a trailing
//     `C:\some dir\` survive intact.
//   - Any other backslash is kept verbatim, which is what a Windows path needs.
func tokenize(input string) ([]string, error) {
	var tokens []string
	var current strings.Builder
	var quote rune
	flush := func() {
		if current.Len() > 0 {
			tokens = append(tokens, current.String())
			current.Reset()
		}
	}
	runes := []rune(input)
	for i := 0; i < len(runes); i++ {
		char := runes[i]
		if quote == '\'' {
			// Single quotes are fully literal.
			if char == '\'' {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			continue
		}
		if char == '\\' {
			// Escape only when the next character would otherwise be special.
			if i+1 < len(runes) {
				next := runes[i+1]
				if next == '"' || next == '\\' || next == ' ' || next == '\t' || next == '\'' {
					current.WriteRune(next)
					i++
					continue
				}
			}
			// A trailing backslash, or one before an ordinary character such as
			// "d" in C:\data, is literal.
			current.WriteRune(char)
			continue
		}
		if quote == '"' {
			if char == '"' {
				quote = 0
			} else {
				current.WriteRune(char)
			}
			continue
		}
		if char == '\'' || char == '"' {
			quote = char
			continue
		}
		if char == ' ' || char == '\t' || char == '\r' || char == '\n' {
			flush()
			continue
		}
		current.WriteRune(char)
	}
	if quote != 0 {
		return nil, errors.New("unterminated quote in command arguments")
	}
	flush()
	return tokens, nil
}
