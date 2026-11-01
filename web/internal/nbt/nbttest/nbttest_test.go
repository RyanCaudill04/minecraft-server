package nbttest_test

import (
	"strings"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/nbt/nbttest"
)

// Input NBT can't represent must panic rather than encode a file the decoder
// would misread.
func TestEncodePanicsOnInvalidInput(t *testing.T) {
	tests := map[string]map[string]any{
		"mixed list":       {"l": []any{int8(1), "two"}},
		"long string":      {"s": strings.Repeat("x", 1<<16)},
		"long key":         {strings.Repeat("k", 1<<16): int8(1)},
		"unsupported type": {"u": uint8(1)},
	}
	for name, root := range tests {
		t.Run(name, func(t *testing.T) {
			defer func() {
				if recover() == nil {
					t.Error("want a panic")
				}
			}()
			nbttest.Encode(root, false)
		})
	}
}
