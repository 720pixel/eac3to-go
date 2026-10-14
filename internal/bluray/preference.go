package bluray

import (
	"fmt"
	"strings"
)

type SelectionOptions struct {
	// Language chooses a picture localization only when authored menu metadata
	// explicitly identifies it. Audio-track language does not identify pictures.
	Language string
}

func SelectMain(playlists []*Playlist) (*Selection, error) {
	return SelectMainWithOptions(playlists, SelectionOptions{})
}

func SelectMainWithOptions(playlists []*Playlist, options SelectionOptions) (*Selection, error) {
	language := strings.ToLower(strings.TrimSpace(options.Language))
	if language != "" {
		if len(language) != 3 {
			return nil, fmt.Errorf("picture language must be a three-letter ISO 639 code")
		}
		for _, c := range language {
			if c < 'a' || c > 'z' {
				return nil, fmt.Errorf("picture language must contain three ASCII letters")
			}
		}
		language = normalizePictureLanguage(language)
	}
	selection, err := selectMain(playlists)
	if err != nil {
		return nil, err
	}
	selection.LanguageRequested = language
	if language == "" {
		return selection, nil
	}
	if selection.Recommended == nil {
		selection.Reasons = append(selection.Reasons, "A language preference cannot resolve the remaining cut, episode, or navigation ambiguity.")
		return selection, nil
	}
	var base *Candidate
	for i := range selection.Candidates {
		if selection.Candidates[i].Playlist.Path == selection.Recommended.Path {
			base = &selection.Candidates[i]
			break
		}
	}
	var chosen *Candidate
	if base != nil && base.Label == "FPL_MainFeature" {
		for i := range selection.Candidates {
			candidate := &selection.Candidates[i]
			if !candidate.Valid || candidate.suspect || candidate.PictureLanguage != language || candidate.LabelSource != base.LabelSource || playlistBDMV(candidate.Playlist.Path) != playlistBDMV(base.Playlist.Path) || candidate.FamilyID == "" || candidate.FamilyID != base.FamilyID {
				continue
			}
			if chosen != nil {
				selection.Recommended, selection.Ambiguous, selection.Confidence = nil, true, "low"
				selection.Reasons = []string{fmt.Sprintf("Several authored picture branches match %s; choose a playlist explicitly.", language)}
				return selection, nil
			}
			chosen = candidate
		}
	}
	if chosen == nil {
		selection.Reasons = append(selection.Reasons, fmt.Sprintf("No unambiguous authored %s picture branch is established; retaining the recommended presentation. Audio-language listings are not picture-language evidence.", language))
		return selection, nil
	}
	selection.Recommended = chosen.Playlist
	selection.LanguageMatched = true
	selection.Reasons = []string{fmt.Sprintf("%s is explicitly labeled %s in %s and matches requested picture language %s; the authored default is %s.", chosen.Playlist.Name, chosen.Label, chosen.LabelSource, language, base.Playlist.Name)}
	return selection, nil
}

func normalizePictureLanguage(language string) string {
	aliases := map[string]string{"fre": "fra", "ger": "deu", "chi": "zho", "dut": "nld", "cze": "ces", "gre": "ell", "rum": "ron", "ice": "isl", "per": "fas"}
	if canonical := aliases[language]; canonical != "" {
		return canonical
	}
	return language
}

func authoredPictureLanguage(label string) string {
	suffix, ok := strings.CutPrefix(label, "FPL_MainFeature_")
	if !ok {
		return ""
	}
	suffix = normalizePictureLanguage(suffix)
	if strings.Contains("|eng|spa|fra|deu|ita|jpn|por|rus|zho|kor|nld|hin|ara|tur|pol|hun|ces|dan|swe|fin|nor|tha|vie|ind|heb|ell|ron|ukr|isl|bul|hrv|slv|srp|cat|tam|tel|mal|mar|urd|pan|fas|ben|", "|"+suffix+"|") {
		return suffix
	}
	return ""
}
