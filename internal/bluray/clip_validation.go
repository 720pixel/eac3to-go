package bluray

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
)

type ClipIssue struct {
	Level   string `json:"level"`
	Code    string `json:"code"`
	ClipID  string `json:"clip_id"`
	Item    int    `json:"item"`
	PID     uint16 `json:"pid,omitempty"`
	Message string `json:"message"`
}

type ClipValidation struct {
	Status          string      `json:"status"`
	TotalClips      int         `json:"total_clips"`
	CheckedClips    int         `json:"checked_clips"`
	Issues          []ClipIssue `json:"issues,omitempty"`
	IssueCount      int         `json:"issue_count"`
	IssuesTruncated bool        `json:"issues_truncated"`
}

// ValidatePlayItem checks the selected main play item against CLPI clocks and
// program streams. Missing PIDs are warnings because authored titles may list
// an optional stream absent from a particular segment. No payload is decoded.
func ValidatePlayItem(item PlayItem, info *ClipInfo) []ClipIssue {
	var issues []ClipIssue
	add := func(level, code, message string, pid uint16) {
		issues = append(issues, ClipIssue{Level: level, Code: code, ClipID: item.ClipID, PID: pid, Message: message})
	}
	if info == nil {
		add("warning", "missing_metadata", "Clip information is unavailable.", 0)
		return issues
	}
	found, fits, wrap := false, false, false
	for _, sequence := range info.Sequences {
		if sequence.STCID != item.STCID {
			continue
		}
		found = true
		wrap = wrap || sequence.ClockWrap
		if !sequence.ClockWrap && item.OutTime >= item.InTime && item.InTime >= sequence.InTime && item.OutTime <= sequence.OutTime {
			fits = true
		}
	}
	switch {
	case !found:
		add("error", "missing_stc", fmt.Sprintf("MPLS STC %d is absent from the CLPI sequence table.", item.STCID), 0)
	case !fits && wrap:
		add("warning", "clock_wrap", "CLPI clock rollover prevents a direct presentation-window check.", 0)
	case !fits:
		add("error", "outside_presentation", fmt.Sprintf("MPLS window [%d,%d) lies outside the CLPI presentation bounds for STC %d.", item.InTime, item.OutTime, item.STCID), 0)
	}
	pids := make(map[uint16]map[uint8]bool)
	for _, program := range info.Programs {
		for _, stream := range program.Streams {
			if pids[stream.PID] == nil {
				pids[stream.PID] = make(map[uint8]bool)
			}
			pids[stream.PID][stream.CodingType] = true
		}
	}
	for _, stream := range item.Streams {
		if stream.EntryType != 1 {
			continue
		}
		if len(pids[stream.PID]) == 0 {
			add("warning", "missing_pid", fmt.Sprintf("MPLS PID 0x%04x is absent from this clip's CLPI program inventory.", stream.PID), stream.PID)
		} else if !pids[stream.PID][stream.CodingType] {
			add("warning", "coding_type", fmt.Sprintf("MPLS and CLPI disagree on the coding type of PID 0x%04x.", stream.PID), stream.PID)
		}
	}
	return issues
}

type clipRequest struct {
	path string
	info *ClipInfo
	err  error
}

func attachClipValidation(selection *Selection) {
	byDisc := make(map[string]map[string]string)
	requests := make(map[string]*clipRequest)
	var work []*clipRequest
	for _, candidate := range selection.Candidates {
		bdmv := playlistBDMV(candidate.Playlist.Path)
		inventory, loaded := byDisc[bdmv]
		if !loaded {
			inventory = make(map[string]string)
			if dir, err := childCaseInsensitive(bdmv, "CLIPINF"); err == nil {
				if entries, err := os.ReadDir(dir); err == nil {
					for _, entry := range entries {
						if entry.IsDir() {
							continue
						}
						name := strings.ToLower(entry.Name())
						if _, exists := inventory[name]; exists {
							inventory[name] = ""
						} else {
							inventory[name] = filepath.Join(dir, entry.Name())
						}
					}
				}
			}
			byDisc[bdmv] = inventory
		}
		for _, item := range candidate.Playlist.Items {
			key := bdmv + "/" + item.ClipID
			if _, exists := requests[key]; exists {
				continue
			}
			path := inventory[item.ClipID+".clpi"]
			if path == "" {
				path = inventory[item.ClipID+".cpi"]
			}
			request := &clipRequest{path: path}
			requests[key] = request
			if path != "" {
				work = append(work, request)
			}
		}
	}
	var workers sync.WaitGroup
	jobs := make(chan *clipRequest)
	for worker := 0; worker < min(8, runtime.GOMAXPROCS(0), len(work)); worker++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			for request := range jobs {
				request.info, request.err = ReadClipInfo(request.path)
				if request.info != nil {
					// Selection retains clocks and PIDs, not seek-map buffers.
					request.info.CPI, request.info.Marks, request.info.Extensions = nil, nil, nil
				}
			}
		}()
	}
	for _, request := range work {
		jobs <- request
	}
	close(jobs)
	workers.Wait()
	for i := range selection.Candidates {
		candidate := &selection.Candidates[i]
		bdmv := playlistBDMV(candidate.Playlist.Path)
		validation := ClipValidation{Status: "unavailable"}
		seen := make(map[string]bool)
		invalid := false
		for itemIndex, item := range candidate.Playlist.Items {
			request := requests[bdmv+"/"+item.ClipID]
			first := !seen[item.ClipID]
			if first {
				seen[item.ClipID] = true
				validation.TotalClips++
				if request.info != nil {
					validation.CheckedClips++
				}
			}
			var issues []ClipIssue
			if request.info != nil {
				issues = ValidatePlayItem(item, request.info)
			} else if first && request.err != nil {
				issues = []ClipIssue{{Level: "warning", Code: "malformed_metadata", ClipID: item.ClipID, Message: request.err.Error()}}
			} else if first {
				issues = []ClipIssue{{Level: "warning", Code: "missing_metadata", ClipID: item.ClipID, Message: "Clip information is unavailable."}}
			}
			for _, issue := range issues {
				issue.Item = itemIndex
				validation.IssueCount++
				invalid = invalid || issue.Level == "error"
				if len(validation.Issues) < 32 {
					validation.Issues = append(validation.Issues, issue)
				}
			}
		}
		if validation.CheckedClips > 0 {
			validation.Status = "partial"
			if validation.CheckedClips == validation.TotalClips && validation.IssueCount == 0 {
				validation.Status = "validated"
			}
		}
		if invalid {
			validation.Status, candidate.Valid = "invalid", false
			candidate.Reasons = append(candidate.Reasons, "CLPI clocks contradict one or more playlist windows or STC references.")
		}
		validation.IssuesTruncated = len(validation.Issues) < validation.IssueCount
		candidate.ClipValidation = validation
	}
}
