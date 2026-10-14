package bluray

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

// MPLS metadata is small. A generous cap prevents accidental reads of a stream
// or device file passed to Read. Parse can be used for a larger in-memory file.
const maxPlaylistBytes = 64 << 20

func Read(path string) (*Playlist, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("playlist %q is not a regular file", path)
	}
	file, err := os.Open(absolute)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	info, err = file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("playlist %q is not a regular file", path)
	}
	if info.Size() > maxPlaylistBytes {
		return nil, fmt.Errorf("playlist %q exceeds %d-byte metadata limit", path, maxPlaylistBytes)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxPlaylistBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxPlaylistBytes {
		return nil, fmt.Errorf("playlist %q exceeds metadata limit", path)
	}
	p, err := Parse(data)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	p.Path, p.Name = absolute, filepath.Base(absolute)
	return p, nil
}

// Discover accepts a disc root, BDMV directory, PLAYLIST directory, or a single
// playlist file. Results are in filename order and include menu/short playlists;
// no title or obfuscation heuristic silently removes entries. On malformed
// files, it returns successfully read playlists together with a joined error.
func Discover(path string) ([]*Playlist, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		p, err := Read(path)
		if err != nil {
			return nil, err
		}
		return []*Playlist{p}, nil
	}
	dir, err := playlistDirectory(path)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	var paths []string
	for _, entry := range entries {
		ext := filepath.Ext(entry.Name())
		if entry.IsDir() || (!strings.EqualFold(ext, ".mpls") && !strings.EqualFold(ext, ".mpl")) {
			continue
		}
		paths = append(paths, filepath.Join(dir, entry.Name()))
	}
	if len(paths) == 0 {
		return nil, fmt.Errorf("no MPLS playlists found in %q", dir)
	}
	// A small worker pool overlaps metadata reads without flooding an optical
	// drive or allocating a buffer for every playlist at once.
	workers := min(runtime.GOMAXPROCS(0), 8, len(paths))
	parsed := make([]*Playlist, len(paths))
	failures := make([]error, len(paths))
	jobs := make(chan int)
	var group sync.WaitGroup
	for worker := 0; worker < workers; worker++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range jobs {
				parsed[index], failures[index] = Read(paths[index])
			}
		}()
	}
	for index := range paths {
		jobs <- index
	}
	close(jobs)
	group.Wait()
	var result []*Playlist
	for _, playlist := range parsed {
		if playlist != nil {
			result = append(result, playlist)
		}
	}
	return result, errors.Join(failures...)
}

func playlistDirectory(path string) (string, error) {
	if strings.EqualFold(filepath.Base(filepath.Clean(path)), "PLAYLIST") {
		return path, nil
	}
	if dir, err := childCaseInsensitive(path, "PLAYLIST"); err == nil {
		return dir, nil
	}
	bdmv, err := childCaseInsensitive(path, "BDMV")
	if err != nil {
		return "", fmt.Errorf("%q is not a Blu-ray disc root, BDMV, or PLAYLIST directory", path)
	}
	return childCaseInsensitive(bdmv, "PLAYLIST")
}

// ResolveClips resolves the selected angle's clips in item order, preserving
// repeated clips. It checks every path exists; callers must apply InTime and
// OutTime when presenting the media, rather than concatenating whole files.
func ResolveClips(p *Playlist) ([]string, error) {
	if p == nil || p.Path == "" {
		return nil, fmt.Errorf("cannot resolve clips without a playlist file path")
	}
	playlistDir := filepath.Dir(p.Path)
	if !strings.EqualFold(filepath.Base(playlistDir), "PLAYLIST") {
		return nil, fmt.Errorf("playlist %q is not inside a PLAYLIST directory", p.Path)
	}
	bdmvDir := filepath.Dir(playlistDir)
	if strings.EqualFold(filepath.Base(bdmvDir), "BACKUP") {
		bdmvDir = filepath.Dir(bdmvDir)
	}
	streamDir, err := childCaseInsensitive(bdmvDir, "STREAM")
	if err != nil {
		return nil, fmt.Errorf("finding streams for %s: %w", p.Name, err)
	}
	result := make([]string, 0, len(p.Items))
	for i, item := range p.Items {
		if err := validateClip(Angle{ClipID: item.ClipID, CodecID: item.CodecID}); err != nil {
			return nil, fmt.Errorf("play item %d: %w", i+1, err)
		}
		ext := ".m2ts"
		if item.CodecID == "FMTS" {
			ext = ".fmts"
		}
		clipPath, err := childCaseInsensitive(streamDir, item.ClipID+ext)
		if err != nil && errors.Is(err, os.ErrNotExist) && ext == ".m2ts" {
			// AVCHD discs may use 8.3 filenames.
			clipPath, err = childCaseInsensitive(streamDir, item.ClipID+".mts")
		}
		if err != nil {
			return nil, fmt.Errorf("play item %d clip %s: %w", i+1, item.ClipID, err)
		}
		info, err := os.Stat(clipPath)
		if err != nil {
			return nil, err
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("clip %q is not a regular file", clipPath)
		}
		absolute, err := filepath.Abs(clipPath)
		if err != nil {
			return nil, err
		}
		result = append(result, absolute)
	}
	return result, nil
}

func (p *Playlist) ResolveClips() ([]string, error) { return ResolveClips(p) }

func childCaseInsensitive(dir, name string) (string, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return "", err
	}
	var found string
	for _, entry := range entries {
		if !strings.EqualFold(entry.Name(), name) {
			continue
		}
		if found != "" {
			return "", fmt.Errorf("ambiguous case-insensitive name %q in %q", name, dir)
		}
		found = filepath.Join(dir, entry.Name())
	}
	if found == "" {
		return "", &os.PathError{Op: "find", Path: filepath.Join(dir, name), Err: os.ErrNotExist}
	}
	return found, nil
}
