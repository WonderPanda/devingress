package ingress

import (
	"fmt"
	"strings"
	"testing"
)

func TestCheckSocketPath(t *testing.T) {
	for _, tc := range []struct {
		goos  string
		limit int
	}{{"linux", 107}, {"darwin", 103}} {
		ok := "/" + strings.Repeat("s", tc.limit-1)
		if err := checkSocketPath(ok, tc.goos); err != nil {
			t.Errorf("%s, %d bytes: %v", tc.goos, len(ok), err)
		}
		long := ok + "s"
		want := fmt.Sprintf("admin socket path %s is %d bytes; Unix sockets allow %d (set a shorter XDG_RUNTIME_DIR)",
			long, tc.limit+1, tc.limit)
		if err := checkSocketPath(long, tc.goos); err == nil || err.Error() != want {
			t.Errorf("%s, %d bytes: %v, want %q", tc.goos, len(long), err, want)
		}
	}
}
