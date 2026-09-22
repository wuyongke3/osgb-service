package main

import (
	"bytes"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func writeTestPLY(t *testing.T, path string, faces [][3]uint32) {
	var buf bytes.Buffer
	buf.WriteString("ply\nformat binary_little_endian 1.0\nelement vertex 5\nproperty float32 x\nproperty float32 y\nproperty float32 z\nelement face 2\nproperty list uchar uint32 vertex_indices\nend_header\n")
	for i := 0; i < 5; i++ {
		binary.Write(&buf, binary.LittleEndian, []float32{float32(i), 0, 0})
	}
	for _, face := range faces {
		buf.WriteByte(3)
		binary.Write(&buf, binary.LittleEndian, face[0])
		binary.Write(&buf, binary.LittleEndian, face[1])
		binary.Write(&buf, binary.LittleEndian, face[2])
	}
	if err := os.WriteFile(path, buf.Bytes(), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestConvertFullmeshFaceIndicesToUnsignedByte(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.ply")
	writeTestPLY(t, path, [][3]uint32{{4, 3, 2}, {1, 0, 4}})
	if err := convertFullmeshFaceIndicesToUnsignedByte(path); err != nil {
		t.Fatal(err)
	}
	header, err := readPlyHeader(path)
	if err != nil {
		t.Fatal(err)
	}
	if header.faceCount != 2 {
		t.Fatalf("face count = %d, want 2", header.faceCount)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Contains(data, []byte("property list uchar int vertex_indices")) {
		t.Fatal("header not converted")
	}
}

func TestValidateOSGBInputPLY(t *testing.T) {
	path := filepath.Join(t.TempDir(), "mesh.ply")
	writeTestPLY(t, path, [][3]uint32{{4, 3, 2}})
	if err := validateOSGBInput(path); err != nil {
		t.Fatal(err)
	}
}

// TestNewIDIsUniqueUnderConcurrency is the regression test for ID generation.
// Job IDs name directories on disk, so two jobs sharing an ID would share a
// workspace and corrupt each other's state. The previous implementation relied
// on a nanosecond timestamp plus a time-seeded LCG and had no uniqueness
// guarantee across concurrent callers.
func TestNewIDIsUniqueUnderConcurrency(t *testing.T) {
	const workers = 16
	const perWorker = 500
	var wg sync.WaitGroup
	results := make(chan string, workers*perWorker)
	for i := 0; i < workers; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := 0; j < perWorker; j++ {
				results <- newID()
			}
		}()
	}
	wg.Wait()
	close(results)

	seen := make(map[string]bool, workers*perWorker)
	for id := range results {
		if id == "" {
			t.Fatal("newID returned an empty string")
		}
		if seen[id] {
			t.Fatalf("duplicate ID generated: %s", id)
		}
		seen[id] = true
	}
	if len(seen) != workers*perWorker {
		t.Fatalf("got %d unique IDs, want %d", len(seen), workers*perWorker)
	}
}

// TestNewIDIsFilesystemSafe guards the constraint that IDs become directory
// names, so they must not contain path separators or other hostile characters.
func TestNewIDIsFilesystemSafe(t *testing.T) {
	for i := 0; i < 200; i++ {
		id := newID()
		if strings.ContainsAny(id, `/\:*?"<>|`) {
			t.Fatalf("ID %q contains a path-hostile character", id)
		}
		if strings.Contains(id, "..") {
			t.Fatalf("ID %q contains a traversal sequence", id)
		}
	}
}

// TestRandomTokenLengthAndAlphabet checks the token contract directly.
func TestRandomTokenLengthAndAlphabet(t *testing.T) {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	for _, n := range []int{0, 1, 6, 32} {
		token := randomToken(n)
		if len(token) != n {
			t.Fatalf("randomToken(%d) length = %d", n, len(token))
		}
		if !strings.Contains(alphabet, token) && n > 0 {
			// Every character must come from the alphabet.
			for _, ch := range token {
				if !strings.ContainsRune(alphabet, ch) {
					t.Fatalf("randomToken(%d) = %q contains %q", n, token, ch)
				}
			}
		}
	}
}

// TestRandomTokenVaries confirms the token is not a fixed sequence, which the
// old deterministic time-seeded LCG could approach under rapid calls.
func TestRandomTokenVaries(t *testing.T) {
	first := randomToken(12)
	identical := 0
	for i := 0; i < 50; i++ {
		if randomToken(12) == first {
			identical++
		}
	}
	if identical > 0 {
		t.Fatalf("randomToken repeated 12-char value %d times", identical)
	}
}

// TestEncodeBase36 covers the small helper used to keep IDs short.
func TestEncodeBase36(t *testing.T) {
	cases := []struct {
		in   uint64
		want string
	}{{0, "0"}, {1, "1"}, {9, "9"}, {10, "a"}, {35, "z"}, {36, "10"}, {1295, "zz"}}
	for _, c := range cases {
		if got := encodeBase36(c.in); got != c.want {
			t.Errorf("encodeBase36(%d) = %q, want %q", c.in, got, c.want)
		}
	}
}
