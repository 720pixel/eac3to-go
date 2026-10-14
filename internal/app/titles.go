package app

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"text/tabwriter"

	"eac3to/internal/bluray"
)

func printTitles(w io.Writer, list []*bluray.Playlist, selection *bluray.Selection, o Options) error {
	if len(list) == 0 {
		return fmt.Errorf("no playlists match the duration limit")
	}
	type title struct {
		Number          int                         `json:"number"`
		Name            string                      `json:"name"`
		Duration        float64                     `json:"duration_seconds"`
		Clips           int                         `json:"clips"`
		Chapters        int                         `json:"chapters"`
		Score           float64                     `json:"score"`
		Valid           bool                        `json:"valid"`
		Recommended     bool                        `json:"recommended"`
		Languages       []string                    `json:"audio_languages,omitempty"`
		Duplicates      []string                    `json:"duplicates,omitempty"`
		Reasons         []string                    `json:"reasons"`
		Label           string                      `json:"menu_label,omitempty"`
		LabelSource     string                      `json:"menu_label_source,omitempty"`
		Family          string                      `json:"family_id,omitempty"`
		Variant         string                      `json:"variant,omitempty"`
		PictureLanguage string                      `json:"picture_language,omitempty"`
		Navigation      []bluray.NavigationEvidence `json:"navigation,omitempty"`
		ClipValidation  bluray.ClipValidation       `json:"clip_validation"`
	}
	var titles []title
	for _, c := range selection.Candidates {
		p := c.Playlist
		entry := title{Number: c.OriginalIndex + 1, Name: p.Name, Duration: p.Duration(), Clips: len(p.Items), Chapters: len(p.Chapters), Score: c.Score, Valid: c.Valid, Recommended: p == selection.Recommended, Duplicates: c.DuplicateNames, Reasons: c.Reasons}
		entry.Label, entry.LabelSource = c.Label, c.LabelSource
		entry.Family, entry.Variant, entry.PictureLanguage, entry.Navigation = c.FamilyID, c.Variant, c.PictureLanguage, c.Navigation
		entry.ClipValidation = c.ClipValidation
		seen := map[string]bool{}
		for _, item := range p.Items {
			for _, st := range item.Streams {
				if st.Kind == "audio" && st.Language != "" && !seen[st.Language] {
					seen[st.Language] = true
					entry.Languages = append(entry.Languages, st.Language)
				}
			}
		}
		titles = append(titles, entry)
	}
	if o.JSON {
		return json.NewEncoder(w).Encode(struct {
			Count           int                       `json:"playlist_count"`
			Confidence      string                    `json:"confidence"`
			Ambiguous       bool                      `json:"ambiguous"`
			Reasons         []string                  `json:"reasons"`
			Titles          []title                   `json:"titles"`
			Warnings        []string                  `json:"metadata_warnings,omitempty"`
			Families        []bluray.PlaylistFamily   `json:"families,omitempty"`
			Navigation      []bluray.NavigationReport `json:"navigation,omitempty"`
			Language        string                    `json:"language_requested,omitempty"`
			LanguageMatched bool                      `json:"language_matched"`
		}{len(list), selection.Confidence, selection.Ambiguous, selection.Reasons, titles, selection.MetadataWarnings, selection.Families, selection.Navigation, selection.LanguageRequested, selection.LanguageMatched})
	}
	fmt.Fprintf(w, "%d playlists, %d distinct presentations\n", len(list), len(titles))
	if selection.Recommended != nil {
		fmt.Fprintf(w, "Recommended: %s (%s confidence)\n", selection.Recommended.Name, selection.Confidence)
	} else {
		fmt.Fprintln(w, "Main feature needs a choice.")
	}
	for _, reason := range selection.Reasons {
		fmt.Fprintln(w, reason)
	}
	for _, warning := range selection.MetadataWarnings {
		fmt.Fprintf(w, "Menu metadata: %s\n", warning)
	}
	if selection.SuspectedObfuscationFamilies > 0 {
		fmt.Fprintf(w, "%d groups contain many reordered clip sequences; file numbering does not establish their playback order.\n", selection.SuspectedObfuscationFamilies)
	}
	fmt.Fprintln(w)
	tw := tabwriter.NewWriter(w, 0, 4, 2, ' ', 0)
	fmt.Fprintln(tw, "TITLE\tPLAYLIST\tDURATION\tCLIPS\tCHAPTERS\tAUDIO\t")
	limit := len(titles)
	if !o.ShowAll && limit > 10 {
		limit = 10
	}
	for _, entry := range titles[:limit] {
		label := entry.Label
		if entry.Recommended {
			label = strings.TrimSpace("recommended " + label)
		}
		if !entry.Valid {
			label = "unavailable"
		}
		if len(entry.Duplicates) > 0 {
			label += fmt.Sprintf(" (%d duplicates)", len(entry.Duplicates))
		}
		fmt.Fprintf(tw, "%d)\t%s\t%s\t%d\t%d\t%s\t%s\n", entry.Number, entry.Name, timestamp(entry.Duration), entry.Clips, entry.Chapters, strings.Join(entry.Languages, ", "), label)
	}
	if err := tw.Flush(); err != nil {
		return err
	}
	if limit < len(titles) {
		fmt.Fprintf(w, "\nShowing the top %d candidates. Use -showall for all %d presentations.\n", limit, len(titles))
	}
	if o.Explain {
		for _, entry := range titles[:limit] {
			fmt.Fprintf(w, "\n%s (score %.1f):\n", entry.Name, entry.Score)
			for _, reason := range entry.Reasons {
				fmt.Fprintf(w, "  - %s\n", reason)
			}
			if entry.LabelSource != "" {
				fmt.Fprintf(w, "  - Menu label %q from %s\n", entry.Label, entry.LabelSource)
			}
			for _, evidence := range entry.Navigation {
				fmt.Fprintf(w, "  - %s: %s\n", evidence.Entry, evidence.Explanation)
			}
			if entry.Variant != "" {
				variant := strings.ReplaceAll(entry.Variant, "_", " ")
				if entry.Family != "" {
					variant += " (" + entry.Family + ")"
				}
				fmt.Fprintf(w, "  - %s\n", variant)
			}
			fmt.Fprintf(w, "  - Clip metadata: %s (%d/%d clips checked).\n", entry.ClipValidation.Status, entry.ClipValidation.CheckedClips, entry.ClipValidation.TotalClips)
			for _, issue := range entry.ClipValidation.Issues {
				fmt.Fprintf(w, "  - Clip %s: %s\n", issue.ClipID, issue.Message)
			}
		}
		for _, family := range selection.Families {
			fmt.Fprintf(w, "\n%s: %s; %d playlists, %d distinct picture orders.\n", family.ID, strings.ReplaceAll(family.Kind, "_", " "), len(family.Members), family.DistinctOrders)
			for _, reason := range family.Reasons {
				fmt.Fprintf(w, "  - %s\n", reason)
			}
			for _, diff := range family.Differences[:min(3, len(family.Differences))] {
				clip := func(id string) string {
					if id == "" {
						return "has ended"
					}
					return "uses " + id + ".m2ts"
				}
				fmt.Fprintf(w, "  - At %s: %s %s; %s %s.\n", timestamp(float64(diff.StartTicks)/45000), diff.LeftPlaylist, clip(diff.LeftClip), diff.RightPlaylist, clip(diff.RightClip))
			}
		}
	}
	fmt.Fprintln(w, "\nInspect: eac3to DISC --main   or   eac3to DISC --playlist=NNNNN")
	fmt.Fprintln(w, "Extract: eac3to DISC -demux -destpath=OUTPUT")
	if selection.Recommended == nil && len(titles) > 1 {
		fmt.Fprintf(w, "Compare: eac3to DISC --compare-playlists=%s,%s\n", strings.TrimSuffix(titles[0].Name, ".mpls"), strings.TrimSuffix(titles[1].Name, ".mpls"))
	}
	return nil
}
