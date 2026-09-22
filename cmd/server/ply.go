package main

// ply.go handles PLY compatibility. OpenMVS emits binary PLY whose face-index
// declarations the OSG 3.6.5 PLY plugin rejects, so the header is rewritten
// before conversion and the input is validated first.

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
)

func validateOSGBInput(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("input model not accessible: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("input model is empty: %s", path)
	}
	switch strings.ToLower(filepath.Ext(path)) {
	case ".ply":
		header, err := readPlyHeader(path)
		if err != nil {
			return err
		}
		if header.vertexCount <= 0 || !header.hasFaces {
			return fmt.Errorf("PLY has no geometry: vertices=%d faces=%d", header.vertexCount, header.faceCount)
		}
		expected := header.payloadOffset + int64(header.vertexCount)*12
		if info.Size() <= expected {
			return fmt.Errorf("PLY is truncated: size=%d expected_face_payload_after=%d", info.Size(), expected)
		}
	case ".obj":
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		scanner := bufio.NewScanner(file)
		scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
		vertices, faces := 0, 0
		for scanner.Scan() {
			line := strings.TrimSpace(scanner.Text())
			if strings.HasPrefix(line, "v ") {
				vertices++
			} else if strings.HasPrefix(line, "f ") {
				faces++
			}
		}
		if err := scanner.Err(); err != nil {
			return fmt.Errorf("scan OBJ: %w", err)
		}
		if vertices == 0 || faces == 0 {
			return fmt.Errorf("OBJ has no geometry: vertices=%d faces=%d", vertices, faces)
		}
	case ".glb":
		file, err := os.Open(path)
		if err != nil {
			return err
		}
		defer file.Close()
		var magic uint32
		if err := binary.Read(file, binary.LittleEndian, &magic); err != nil || magic != 0x46546C67 {
			return fmt.Errorf("invalid GLB magic")
		}
	default:
		return fmt.Errorf("unsupported model format: %s", filepath.Ext(path))
	}
	return nil
}

type osgbOutputStats struct {
	Vertices    int64 `json:"vertices"`
	Indices     int64 `json:"indices"`
	Faces       int64 `json:"faces"`
	Geodes      int64 `json:"geodes"`
	Tiles       int64 `json:"tiles,omitempty"`
	ValidTiles  int64 `json:"valid_tiles,omitempty"`
	FailedTiles int64 `json:"failed_tiles,omitempty"`
	Textured    bool  `json:"textured,omitempty"`
}

func parseOSGTStats(path string) (osgbOutputStats, error) {
	stats := osgbOutputStats{}
	file, err := os.Open(path)
	if err != nil {
		return stats, err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 1024*1024), 1024*1024)
	inElementVector := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if strings.HasPrefix(line, "osg::Geode ") {
			stats.Geodes++
			inElementVector = false
			continue
		}
		if strings.HasPrefix(line, "osg::DrawElements") {
			inElementVector = true
			continue
		}
		if strings.HasPrefix(line, "vector ") && inElementVector {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if value, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil {
					stats.Indices = value
					stats.Faces = value / 3
				}
			}
			inElementVector = false
			continue
		}
		if strings.HasPrefix(line, "vector ") && strings.Contains(line, "Vec3Array") {
			continue
		}
		if strings.HasPrefix(line, "Count ") {
			fields := strings.Fields(line)
			if len(fields) >= 2 {
				if value, parseErr := strconv.ParseInt(fields[1], 10, 64); parseErr == nil {
					if value > stats.Vertices {
						stats.Vertices = value
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return stats, fmt.Errorf("scan OSGB text export: %w", err)
	}
	return stats, nil
}

func (m *JobManager) validateOSGBOutput(id, path, sourcePath string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("OSGB not found: %w", err)
	}
	if info.Size() == 0 {
		return fmt.Errorf("OSGB is empty: %s", path)
	}
	osgt := path + ".validate.osgt"
	osgconv := m.config.OSGConvBin
	if osgconv == "" {
		return fmt.Errorf("OSGCONV_BIN is not configured")
	}
	defer os.Remove(osgt)
	cmd := exec.Command(osgconv, path, osgt)
	cmd.Stdout = io.Discard
	cmd.Stderr = io.Discard
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("OSGB cannot be read back: %w", err)
	}
	stats, err := parseOSGTStats(osgt)
	if err != nil {
		return fmt.Errorf("inspect OSGB: %w", err)
	}
	if stats.Vertices <= 0 {
		return fmt.Errorf("OSGB contains no vertices")
	}
	if stats.Indices <= 0 || stats.Faces <= 0 {
		return fmt.Errorf("OSGB contains no triangles: indices=%d faces=%d", stats.Indices, stats.Faces)
	}
	if !strings.EqualFold(filepath.Ext(sourcePath), ".ply") {
		header, headerErr := readPlyHeader(sourcePath)
		if headerErr == nil && header.hasFaces && header.faceCount > 0 && stats.Faces != int64(header.faceCount) {
			// OSG may append a sentinel/deduplicated primitive record, so only
			// fail when the output is materially incomplete.
			tolerance := int64(header.faceCount)/1000 + 8
			if stats.Faces < int64(header.faceCount)-tolerance {
				return fmt.Errorf("OSGB face mismatch: got %d want at least %d", stats.Faces, int64(header.faceCount)-tolerance)
			}
		}
		m.updateJobStats(id, sourcePath, &stats)
		return nil
	}
	header, err := readPlyHeader(sourcePath)
	if err != nil {
		return err
	}
	// OpenMVS PLY files can contain a small amount of trailing geometry that
	// osgconv materializes. Reject only materially incomplete outputs.
	vertexTolerance := int64(header.vertexCount)/1000 + 8
	if stats.Vertices < int64(header.vertexCount)-vertexTolerance {
		return fmt.Errorf("OSGB vertex mismatch: got %d want at least %d", stats.Vertices, int64(header.vertexCount)-vertexTolerance)
	}
	faceTolerance := int64(header.faceCount)/1000 + 8
	if stats.Faces < int64(header.faceCount)-faceTolerance {
		return fmt.Errorf("OSGB face mismatch: got %d want at least %d", stats.Faces, int64(header.faceCount)-faceTolerance)
	}
	m.updateJobStats(id, sourcePath, &stats)
	return nil
}

func (m *JobManager) updateJobStats(id, sourcePath string, stats *osgbOutputStats) {
	if stats == nil {
		return
	}
	captured := *stats
	m.update(id, func(job *Job) {
		value := captured
		job.Stats = &value
	})
	m.logLine(id, "system", fmt.Sprintf("OSGB validation passed: vertices=%d indices=%d faces=%d geodes=%d source=%s", stats.Vertices, stats.Indices, stats.Faces, stats.Geodes, sourcePath))
}

// offsets remain valid because the original header length is recorded before
// rewriting and used as the source seek offset when copying the payload.
type plyElement struct {
	name  string
	count int
}

type plyHeader struct {
	format         string
	vertexCount    int
	faceCount      int
	elements       []plyElement
	hasFaces       bool
	vertexHasColor bool
	faceIndexSize  int
	payloadOffset  int64
	raw            string
}

func readPlyHeader(path string) (*plyHeader, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	reader := bufio.NewReader(file)
	var sb strings.Builder
	offset := 0
	header := &plyHeader{}
	for {
		line, readErr := reader.ReadString('\n')
		sb.WriteString(line)
		offset += len(line)
		text := strings.TrimRight(line, "\r\n")
		fields := strings.Fields(text)
		switch {
		case len(fields) >= 2 && fields[0] == "format":
			header.format = fields[1]
		case len(fields) >= 3 && fields[0] == "element":
			count, _ := strconv.Atoi(fields[2])
			header.elements = append(header.elements, plyElement{name: fields[1], count: count})
			if fields[1] == "vertex" {
				header.vertexCount = count
			} else if fields[1] == "face" {
				header.faceCount = count
				header.hasFaces = count > 0
			}
		case len(fields) >= 3 && fields[0] == "property" && fields[1] != "list" && currentElement(header.elements) == "vertex":
			lower := strings.ToLower(text)
			if strings.Contains(lower, "red") || strings.Contains(lower, "green") || strings.Contains(lower, "blue") {
				header.vertexHasColor = true
			}
		case len(fields) >= 4 && fields[0] == "property" && fields[1] == "list" && currentElement(header.elements) == "face":
			size := sizeOfType(fields[3])
			if size > header.faceIndexSize {
				header.faceIndexSize = size
			}
		case strings.TrimSpace(text) == "end_header":
			header.raw = sb.String()
			header.payloadOffset = int64(offset)
			return header, nil
		}
		if readErr != nil {
			if errors.Is(readErr, io.EOF) {
				return nil, fmt.Errorf("read PLY header %s: missing end_header", path)
			}
			return nil, fmt.Errorf("read PLY header %s: %w", path, readErr)
		}
	}
}

func currentElement(elements []plyElement) string {
	if len(elements) == 0 {
		return ""
	}
	return elements[len(elements)-1].name
}

func sizeOfType(name string) int {
	switch strings.ToLower(name) {
	case "char", "uchar", "int8", "uint8":
		return 1
	case "short", "ushort", "int16", "uint16":
		return 2
	case "int", "uint", "int32", "uint32", "float", "float32":
		return 4
	case "double", "float64":
		return 8
	default:
		return 0
	}
}

func convertFullmeshFaceIndicesToUnsignedByte(path string) error {
	header, err := readPlyHeader(path)
	if err != nil {
		return err
	}
	if header.format != "binary_little_endian" || !header.hasFaces {
		return fmt.Errorf("unsupported PLY for face-index conversion: %s", path)
	}

	// Find the exact face list declaration. OpenMVS writes a one-byte vertex
	// count followed by four-byte indices, but historically declares the list
	// as "uint8 uint32". OSG 3.6.5 only understands the canonical spellings
	// "uchar int" for this payload, so rewrite the declaration while keeping
	// the binary payload unchanged whenever it is already valid.
	countSize, indexSize, faceProperty, err := plyFaceProperty(header.raw)
	if err != nil {
		return err
	}
	if countSize != 1 && countSize != 4 {
		return fmt.Errorf("unsupported PLY face count size %d: %s", countSize, path)
	}
	if indexSize != 4 {
		return fmt.Errorf("unsupported PLY face index size %d: %s", indexSize, path)
	}
	if countSize == 4 {
		// Legacy OpenMVS files can declare a four-byte count. Normalize those
		// records to the compact one-byte form expected by OSG.
		return rewriteFaceCountsToUint8(path, header, faceProperty)
	}

	target := strings.Replace(faceProperty, "uint8 uint32", "uchar int", 1)
	target = strings.Replace(target, "uchar uint32", "uchar int", 1)
	target = strings.Replace(target, "uint8 int", "uchar int", 1)
	if target == faceProperty {
		return nil
	}
	rewritten := strings.Replace(header.raw, faceProperty, target, 1)
	temp := path + ".osgply"
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		source.Close()
		return err
	}
	if _, err := out.WriteString(rewritten); err != nil {
		out.Close()
		source.Close()
		os.Remove(temp)
		return err
	}
	if _, err := source.Seek(header.payloadOffset, io.SeekStart); err != nil {
		out.Close()
		source.Close()
		os.Remove(temp)
		return err
	}
	if _, err := io.Copy(out, source); err != nil {
		out.Close()
		source.Close()
		os.Remove(temp)
		return fmt.Errorf("copy PLY payload: %w", err)
	}
	if err := out.Close(); err != nil {
		source.Close()
		os.Remove(temp)
		return err
	}
	if err := source.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	// Best effort: preserve the original file mode. A failure here only affects
	// permissions, never the data, so it must not abort the rewrite.
	if info, statErr := os.Stat(path); statErr == nil {
		_ = os.Chmod(temp, info.Mode())
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}

func plyFaceProperty(raw string) (int, int, string, error) {
	for _, line := range strings.Split(raw, "\n") {
		text := strings.TrimRight(line, "\r\n")
		fields := strings.Fields(text)
		if len(fields) >= 5 && fields[0] == "property" && fields[1] == "list" && fields[4] == "vertex_indices" {
			return sizeOfType(fields[2]), sizeOfType(fields[3]), text, nil
		}
	}
	return 0, 0, "", fmt.Errorf("PLY has no vertex_indices face property")
}

func rewriteFaceCountsToUint8(path string, header *plyHeader, faceProperty string) error {
	temp := path + ".u8ply"
	source, err := os.Open(path)
	if err != nil {
		return err
	}
	defer source.Close()
	if _, err := source.Seek(header.payloadOffset, io.SeekStart); err != nil {
		return err
	}
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return err
	}
	target := strings.Replace(faceProperty, "uint8 uint32", "uchar int", 1)
	target = strings.Replace(target, "uchar uint32", "uchar int", 1)
	target = strings.Replace(target, "uint8 int", "uchar int", 1)
	rewritten := strings.Replace(header.raw, faceProperty, target, 1)
	if _, err := out.WriteString(rewritten); err != nil {
		out.Close()
		os.Remove(temp)
		return err
	}
	writer := bufio.NewWriterSize(out, 1024*1024)
	if _, err := io.CopyN(writer, source, int64(header.vertexCount)*12); err != nil {
		out.Close()
		os.Remove(temp)
		return fmt.Errorf("copy PLY vertex payload: %w", err)
	}
	for i := 0; i < header.faceCount; i++ {
		var count uint32
		if err := binary.Read(source, binary.LittleEndian, &count); err != nil {
			out.Close()
			os.Remove(temp)
			return fmt.Errorf("read face count at face %d: %w", i, err)
		}
		if count > 255 {
			out.Close()
			os.Remove(temp)
			return fmt.Errorf("face %d has %d vertices; cannot encode count as uint8", i, count)
		}
		if err := writer.WriteByte(uint8(count)); err != nil {
			out.Close()
			os.Remove(temp)
			return err
		}
		if _, err := io.CopyN(writer, source, int64(count)*4); err != nil {
			out.Close()
			os.Remove(temp)
			return fmt.Errorf("copy indices at face %d: %w", i, err)
		}
	}
	if err := writer.Flush(); err != nil {
		out.Close()
		os.Remove(temp)
		return err
	}
	if err := out.Close(); err != nil {
		os.Remove(temp)
		return err
	}
	// Best effort: preserve the original file mode. A failure here only affects
	// permissions, never the data, so it must not abort the rewrite.
	if info, statErr := os.Stat(path); statErr == nil {
		_ = os.Chmod(temp, info.Mode())
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return err
	}
	return nil
}
func normalizeOpenMVSPly(path string) (string, error) {
	file, err := os.Open(path)
	if err != nil {
		return "", err
	}
	reader := bufio.NewReader(file)
	var lines []string
	var originalHeaderLen int
	changed := false
	for {
		line, readErr := reader.ReadString('\n')
		originalHeaderLen += len(line)
		text := strings.TrimRight(line, "\r\n")
		if fields := strings.Fields(text); len(fields) >= 3 && fields[0] == "property" {
			if fields[1] == "list" {
				if len(fields) >= 4 {
					fields[2] = normalizePlyTypeName(fields[2])
					fields[3] = normalizePlyTypeName(fields[3])
				}
			} else {
				fields[1] = normalizePlyTypeName(fields[1])
			}
			newText := strings.Join(fields, " ")
			changed = changed || newText != text
			text = newText
		}
		lines = append(lines, text)
		if strings.TrimSpace(line) == "end_header" {
			break
		}
		if readErr != nil {
			file.Close()
			return "", fmt.Errorf("read PLY header %s: %w", path, readErr)
		}
	}
	file.Close()
	if !changed {
		return path, nil
	}
	header := []byte(strings.Join(lines, "\n") + "\n")
	temp := path + ".osgcompat"
	out, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return "", err
	}
	if _, err := out.Write(header); err != nil {
		out.Close()
		os.Remove(temp)
		return "", err
	}
	source, err := os.Open(path)
	if err != nil {
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if _, err := source.Seek(int64(originalHeaderLen), io.SeekStart); err != nil {
		source.Close()
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if _, err := io.Copy(out, source); err != nil {
		source.Close()
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if err := source.Close(); err != nil {
		out.Close()
		os.Remove(temp)
		return "", err
	}
	if err := out.Close(); err != nil {
		os.Remove(temp)
		return "", err
	}
	if err := os.Rename(temp, path); err != nil {
		os.Remove(temp)
		return "", err
	}
	return path, nil
}

func normalizePlyTypeName(name string) string {
	switch name {
	case "uint8":
		return "uchar"
	case "uint16":
		return "ushort"
	case "uint32":
		return "int"
	case "int8":
		return "char"
	case "int16":
		return "short"
	default:
		return name
	}
}

// runNative uses the conventional COLMAP -> OpenMVS -> OpenSceneGraph chain.
// The binaries are intentionally configurable so a production deployment can pin
// versions and GPU-enabled builds without changing the service.
