package routefile

import (
	"reflect"
	"regexp"
	"strings"
	"testing"
)

// TestHelpCoversEveryKey: every TOML key of the route file is documented in
// Help, in the section for its table.
func TestHelpCoversEveryKey(t *testing.T) {
	top, _, _ := strings.Cut(Help, "\n[[")
	checkKeys(t, "top level", top, reflect.TypeFor[file]())
}

func checkKeys(t *testing.T, where, section string, typ reflect.Type) {
	t.Helper()
	for f := range typ.Fields() {
		key := f.Tag.Get("toml")
		if f.Type.Kind() == reflect.Slice && f.Type.Elem().Kind() == reflect.Struct {
			_, table, ok := strings.Cut(Help, "\n[["+key+"]]:")
			if !ok {
				t.Errorf("Help has no [[%s]] section", key)
				continue
			}
			table, _, _ = strings.Cut(table, "\n[[")
			checkKeys(t, "[["+key+"]]", table, f.Type.Elem())
			continue
		}
		// A reference line: the key, then its type. The example's "key = ..." lines don't count.
		if !regexp.MustCompile(`\n  ` + key + ` +(string|array|boolean)`).MatchString(section) {
			t.Errorf("Help does not document %s key %q", where, key)
		}
	}
}

func TestHelpWidth(t *testing.T) {
	for i, line := range strings.Split(Help, "\n") {
		if len(line) > 100 {
			t.Errorf("Help line %d is %d columns: %q", i+1, len(line), line)
		}
	}
}
