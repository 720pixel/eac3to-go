package truehd

import (
	"fmt"
	"path/filepath"
	"strings"
)

// DelayPath carries a lossless TrueHD delay in the output filename, matching
// eac3to's raw THD output. TrueHD access units remain unchanged: inserting or
// discarding them is not the original initial-delay policy for this output.
// The caller supplies the final integer milliseconds after source-delay
// sentinel handling and any explicit delay summation.
func DelayPath(path string, delayMilliseconds int64) (string, error) {
	ext := filepath.Ext(path)
	if !strings.EqualFold(ext, ".thd") {
		return "", fmt.Errorf("lossless TrueHD delay metadata requires a .thd output")
	}
	if delayMilliseconds == 0 {
		return path, nil
	}
	return strings.TrimSuffix(path, ext) + fmt.Sprintf(" DELAY %dms", delayMilliseconds) + ext, nil
}
