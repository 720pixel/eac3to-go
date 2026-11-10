package toolinstall

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/ulikunitz/xz"
)

func archiveFixture(t *testing.T, zipped bool, bad string) []byte {
	t.Helper()
	var b bytes.Buffer
	names := []string{"pair/bin/ffmpeg", "pair/bin/ffprobe", "pair/LICENSE.txt"}
	if zipped {
		names[0] += ".exe"
		names[1] += ".exe"
	}
	if bad != "" {
		names[0] = bad
	}
	if zipped {
		w := zip.NewWriter(&b)
		for _, name := range names {
			f, err := w.Create(name)
			if err != nil {
				t.Fatal(err)
			}
			f.Write([]byte("fixture"))
		}
		if err := w.Close(); err != nil {
			t.Fatal(err)
		}
	} else {
		compressed, err := xz.NewWriter(&b)
		if err != nil {
			t.Fatal(err)
		}
		w := tar.NewWriter(compressed)
		for _, name := range names {
			if err = w.WriteHeader(&tar.Header{Name: name, Size: 7, Mode: 0755}); err != nil {
				t.Fatal(err)
			}
			w.Write([]byte("fixture"))
		}
		w.Close()
		compressed.Close()
	}
	return b.Bytes()
}

func TestInstallerChecksumAtomicPublicationAndCleanup(t *testing.T) {
	for _, zipped := range []bool{false, true} {
		data := archiveFixture(t, zipped, "")
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.Write(data) }))
		defer server.Close()
		target, name := "linux-amd64", "pair.tar.xz"
		if zipped {
			target, name = "windows-amd64", "pair.zip"
		}
		asset := Asset{Name: name, URL: server.URL, Digest: fmt.Sprintf("sha256:%x", sha256.Sum256(data))}
		for _, mode := range []string{"success", "checksum", "runtime", "existing"} {
			dir := t.TempDir()
			destination := filepath.Join(dir, "tools")
			record := asset
			if mode == "checksum" {
				record.Digest = "sha256:" + strings.Repeat("0", 64)
			}
			if mode == "existing" {
				os.Mkdir(destination, 0755)
				os.WriteFile(filepath.Join(destination, "keep"), []byte("original"), 0600)
			}
			verified := false
			_, err := installAsset(context.Background(), destination, target, record, io.Discard, func(_ context.Context, stage, gotTarget string) error {
				verified = true
				if mode == "runtime" {
					return fmt.Errorf("not runnable")
				}
				if gotTarget != target {
					t.Fatal(gotTarget)
				}
				return nil
			})
			if mode == "success" {
				if err != nil || !verified {
					t.Fatalf("%s: %v", mode, err)
				}
				if _, err = os.Stat(filepath.Join(destination, "INSTALL.json")); err != nil {
					t.Fatal(err)
				}
			} else if err == nil {
				t.Fatalf("accepted %s", mode)
			}
			if mode == "checksum" && verified {
				t.Fatal("executed unverified binaries")
			}
			entries, _ := os.ReadDir(dir)
			for _, entry := range entries {
				if strings.HasPrefix(entry.Name(), ".eac3to-tools-") {
					t.Fatal("left temporary download files")
				}
			}
			if mode == "existing" {
				got, _ := os.ReadFile(filepath.Join(destination, "keep"))
				if string(got) != "original" {
					t.Fatal("overwrote existing tools")
				}
			}
		}
	}
}

func TestArchiveTraversalAndMissingTools(t *testing.T) {
	for _, zipped := range []bool{false, true} {
		for _, name := range []string{"pair/bin/../../ffmpeg", "pair/ffmpeg"} {
			data := archiveFixture(t, zipped, name)
			root := t.TempDir()
			archive := filepath.Join(root, "archive")
			os.WriteFile(archive, data, 0600)
			destination := filepath.Join(root, "tools")
			os.Mkdir(destination, 0755)
			target := "linux-amd64"
			if zipped {
				target = "windows-amd64"
			}
			if err := extract(context.Background(), archive, destination, target, zipped); err == nil {
				t.Fatal("accepted malformed dependency archive")
			}
		}
	}
}

func TestTrustedDownloadURL(t *testing.T) {
	const asset = "/releases/download/latest/ffmpeg-n9.0-latest-linux64-gpl-9.0.tar.xz"
	for _, raw := range []string{
		"https://github.com/FFmpeg-Builds/FFmpeg-Builds" + asset,
		"https://github.com/BtbN/FFmpeg-Builds" + asset,
	} {
		if !trustedDownloadURL(raw) {
			t.Errorf("rejected publisher URL %s", raw)
		}
	}
	for _, raw := range []string{
		"http://github.com/FFmpeg-Builds/FFmpeg-Builds" + asset,
		"https://github.com.example.net/FFmpeg-Builds/FFmpeg-Builds" + asset,
		"https://github.com:8443/FFmpeg-Builds/FFmpeg-Builds" + asset,
		"https://user@github.com/FFmpeg-Builds/FFmpeg-Builds" + asset,
		"https://github.com/FFmpeg-Builds/FFmpeg-Builds-mirror" + asset,
		"https://github.com/FFmpeg-Builds-mirror/FFmpeg-Builds" + asset,
		"https://github.com/BtbN/FFmpeg-Builds.evil" + asset,
		"https://github.com/ffmpeg-builds/ffmpeg-builds" + asset,
		"https://github.com/FFmpeg-Builds/FFmpeg-Builds/archive/refs/tags/latest.tar.gz",
		"https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/",
		"https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/../../../../other/repo/releases/download/latest/a.tar.xz",
		"https://github.com/FFmpeg-Builds/FFmpeg-Builds/releases/download/latest%2F..%2F..%2Fother.tar.xz",
		"https://objects.githubusercontent.com/FFmpeg-Builds/FFmpeg-Builds" + asset,
		"https://github.com/other/FFmpeg-Builds" + asset,
		"github.com/FFmpeg-Builds/FFmpeg-Builds" + asset,
		"",
	} {
		if trustedDownloadURL(raw) {
			t.Errorf("accepted untrusted URL %q", raw)
		}
	}
}
