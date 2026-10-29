package sandbox

import (
	"reflect"
	"sort"
	"strings"
	"testing"

	migrations "github.com/zhangzhe-ctrl/ani-resource-service/migrations/sandbox"
)

// Every embedded migration must be executed and checked for readiness, in
// version order. This catches a new SQL file omitted from schemaFiles.
func TestAllEmbeddedMigrationsRegistered(t *testing.T) {
	entries, err := migrations.Files.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)
	if !reflect.DeepEqual(schemaFiles, names) {
		t.Fatalf("registered migrations %v differ from embedded migrations %v", schemaFiles, names)
	}
}
