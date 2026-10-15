// Package toolinstall installs a separately downloaded FFmpeg/FFprobe pair.
package toolinstall

import (
	"archive/tar"
	"archive/zip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/ulikunitz/xz"
)

const apiURL = "https://api.github.com/repos/BtbN/FFmpeg-Builds/releases/tags/latest"
const maxArchive int64 = 512 << 20

var client = &http.Client{Timeout: 10 * time.Minute}

type Asset struct {
	Name   string `json:"name"`
	URL    string `json:"browser_download_url"`
	Digest string `json:"digest"`
}

func DefaultDir() (string, error) {
	base, err := os.UserCacheDir()
	if err != nil {
		return "", err
	}
	return filepath.Join(base, "eac3to-go", "tools"), nil
}

func request(ctx context.Context, url string) (*http.Response, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", "eac3to-go tool installer")
	resp, err := client.Do(req)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		resp.Body.Close()
		return nil, fmt.Errorf("download returned HTTP %d", resp.StatusCode)
	}
	return resp, nil
}

func latestAsset(ctx context.Context, target string) (Asset, error) {
	name := map[string]string{"linux-amd64": "ffmpeg-n9.0-latest-linux64-gpl-9.0.tar.xz", "windows-amd64": "ffmpeg-n9.0-latest-win64-gpl-9.0.zip"}[target]
	if name == "" {
		return Asset{}, fmt.Errorf("automatic FFmpeg download is unavailable for %s; install FFmpeg and FFprobe manually", target)
	}
	resp, err := request(ctx, apiURL)
	if err != nil {
		return Asset{}, err
	}
	defer resp.Body.Close()
	var release struct {
		Assets []Asset `json:"assets"`
	}
	if err = json.NewDecoder(io.LimitReader(resp.Body, 4<<20)).Decode(&release); err != nil {
		return Asset{}, err
	}
	for _, asset := range release.Assets {
		if asset.Name != name {
			continue
		}
		u, err := url.Parse(asset.URL)
		if err != nil || u.Scheme != "https" || u.Host != "github.com" || !strings.HasPrefix(u.Path, "/BtbN/FFmpeg-Builds/releases/download/") {
			return Asset{}, fmt.Errorf("unexpected FFmpeg download URL")
		}
		sum, err := hex.DecodeString(strings.TrimPrefix(asset.Digest, "sha256:"))
		if !strings.HasPrefix(asset.Digest, "sha256:") || err != nil || len(sum) != sha256.Size {
			return Asset{}, fmt.Errorf("publisher did not supply an SHA-256 checksum")
		}
		return asset, nil
	}
	return Asset{}, fmt.Errorf("the FFmpeg publisher has no supported %s asset; install a pair manually", target)
}

func Install(ctx context.Context, directory string, log io.Writer) (string, error) {
	target := runtime.GOOS + "-" + runtime.GOARCH
	asset, err := latestAsset(ctx, target)
	if err != nil {
		return "", err
	}
	return installAsset(ctx, directory, target, asset, log, verifyTools)
}

func installAsset(ctx context.Context, directory, target string, asset Asset, log io.Writer, verify func(context.Context, string, string) error) (string, error) {
	destination, err := filepath.Abs(directory)
	if err != nil {
		return "", err
	}
	if _, err = os.Lstat(destination); err == nil {
		return "", fmt.Errorf("tool directory already exists: %s; choose a new --tools-dir or use the existing pair", destination)
	} else if !os.IsNotExist(err) {
		return "", err
	}
	if err = os.MkdirAll(filepath.Dir(destination), 0755); err != nil {
		return "", err
	}
	stage, err := os.MkdirTemp(filepath.Dir(destination), ".eac3to-tools-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	archive := filepath.Join(stage, "download")
	fmt.Fprintln(log, "Downloading a matched FFmpeg/FFprobe pair from BtbN/FFmpeg-Builds…")
	resp, err := request(ctx, asset.URL)
	if err != nil {
		return "", err
	}
	file, err := os.Create(archive)
	if err != nil {
		resp.Body.Close()
		return "", err
	}
	hash := sha256.New()
	n, copyErr := io.Copy(io.MultiWriter(file, hash), io.LimitReader(resp.Body, maxArchive+1))
	closeErr := file.Close()
	resp.Body.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	if n > maxArchive {
		return "", fmt.Errorf("FFmpeg archive exceeds download limit")
	}
	if "sha256:"+hex.EncodeToString(hash.Sum(nil)) != asset.Digest {
		return "", fmt.Errorf("FFmpeg archive checksum mismatch; nothing was installed")
	}
	extracted := filepath.Join(stage, "tools")
	if err = os.Mkdir(extracted, 0755); err != nil {
		return "", err
	}
	fmt.Fprintln(log, "Checksum verified; extracting and checking the programs…")
	if err = extract(ctx, archive, extracted, target, strings.HasSuffix(asset.Name, ".zip")); err != nil {
		return "", err
	}
	if err = verify(ctx, extracted, target); err != nil {
		return "", err
	}
	manifest, err := json.MarshalIndent(struct {
		Publisher string `json:"publisher"`
		Target    string `json:"target"`
		Asset     Asset  `json:"asset"`
	}{"BtbN/FFmpeg-Builds", target, asset}, "", "  ")
	if err != nil {
		return "", err
	}
	if err = os.WriteFile(filepath.Join(extracted, "INSTALL.json"), append(manifest, '\n'), 0644); err != nil {
		return "", err
	}
	if err = ctx.Err(); err != nil {
		return "", err
	}
	if err = os.Rename(extracted, destination); err != nil {
		return "", err
	}
	fmt.Fprintln(log, "Installed:", destination)
	return destination, nil
}

func extract(ctx context.Context, archive, directory, target string, zipped bool) error {
	suffix := ""
	if strings.HasPrefix(target, "windows-") {
		suffix = ".exe"
	}
	wanted := map[string]bool{"ffmpeg" + suffix: false, "ffprobe" + suffix: false}
	notices := 0
	write := func(name string, size int64, regular bool, reader io.Reader) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		base := path.Base(name)
		_, tool := wanted[base]
		notice := strings.HasPrefix(strings.ToUpper(base), "LICENSE") || strings.HasPrefix(strings.ToUpper(base), "COPYING") || strings.HasPrefix(strings.ToUpper(base), "NOTICE")
		if !tool && !notice {
			return nil
		}
		if !regular || strings.Contains(name, "\\") || strings.HasPrefix(name, "/") {
			return fmt.Errorf("invalid FFmpeg archive entry")
		}
		for _, part := range strings.Split(name, "/") {
			if part == ".." {
				return fmt.Errorf("unsafe FFmpeg archive entry")
			}
		}
		if tool && (!strings.HasSuffix(name, "/bin/"+base) || wanted[base]) {
			return fmt.Errorf("duplicate or misplaced FFmpeg program")
		}
		if size < 1 || size > 256<<20 || notice && size > 2<<20 {
			return fmt.Errorf("invalid FFmpeg archive file size")
		}
		mode := os.FileMode(0644)
		destination := filepath.Join(directory, base)
		if tool {
			mode = 0755
		} else {
			notices++
			destination = filepath.Join(directory, fmt.Sprintf("NOTICE-%d-%s", notices, base))
		}
		file, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
		if err != nil {
			return err
		}
		n, copyErr := io.Copy(file, io.LimitReader(reader, size+1))
		closeErr := file.Close()
		if copyErr != nil {
			return copyErr
		}
		if closeErr != nil {
			return closeErr
		}
		if n != size {
			return fmt.Errorf("FFmpeg archive entry size mismatch")
		}
		if tool {
			wanted[base] = true
		}
		return nil
	}
	if zipped {
		archive, err := zip.OpenReader(archive)
		if err != nil {
			return err
		}
		defer archive.Close()
		if len(archive.File) > 10000 {
			return fmt.Errorf("too many FFmpeg archive entries")
		}
		for _, entry := range archive.File {
			if entry.UncompressedSize64 > 1<<30 {
				return fmt.Errorf("FFmpeg archive entry exceeds limit")
			}
			r, err := entry.Open()
			if err != nil {
				return err
			}
			err = write(entry.Name, int64(entry.UncompressedSize64), entry.Mode().IsRegular(), r)
			r.Close()
			if err != nil {
				return err
			}
		}
	} else {
		file, err := os.Open(archive)
		if err != nil {
			return err
		}
		defer file.Close()
		decompressed, err := xz.NewReader(file)
		if err != nil {
			return err
		}
		reader := tar.NewReader(io.LimitReader(decompressed, 2<<30))
		for count := 0; ; count++ {
			if count > 10000 {
				return fmt.Errorf("too many FFmpeg archive entries")
			}
			entry, err := reader.Next()
			if err == io.EOF {
				break
			}
			if err != nil {
				return err
			}
			if err = write(entry.Name, entry.Size, entry.Typeflag == tar.TypeReg, reader); err != nil {
				return err
			}
		}
	}
	for name, found := range wanted {
		if !found {
			return fmt.Errorf("FFmpeg archive is missing %s", name)
		}
	}
	if notices == 0 {
		return fmt.Errorf("FFmpeg archive is missing license notices")
	}
	return nil
}

func verifyTools(ctx context.Context, directory, target string) error {
	suffix := ""
	if strings.HasPrefix(target, "windows-") {
		suffix = ".exe"
	}
	bounded, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for _, name := range []string{"ffmpeg", "ffprobe"} {
		data, err := exec.CommandContext(bounded, filepath.Join(directory, name+suffix), "-version").Output()
		if err != nil || !strings.HasPrefix(string(data), name+" version ") {
			return fmt.Errorf("downloaded %s failed its runtime check: %v", name, err)
		}
	}
	data, err := exec.CommandContext(bounded, filepath.Join(directory, "ffprobe"+suffix), "-v", "error", "-protocols").Output()
	if err != nil {
		return fmt.Errorf("downloaded FFprobe protocol check: %w", err)
	}
	for _, word := range strings.Fields(string(data)) {
		if word == "bluray" {
			return nil
		}
	}
	return fmt.Errorf("downloaded FFprobe lacks Blu-ray/libbluray support; nothing was installed")
}
