package main

import (
	"errors"
	"strings"
	"testing"

	"github.com/ryancaudill04/minecraft-server/web/internal/pifirmware"
)

func TestReadInput(t *testing.T) {
	var s pifirmware.Settings
	if err := readInput(strings.NewReader(`{"arm_freq": 2600, "temp_limit": 80}`), &s); err != nil {
		t.Fatal(err)
	}
	if *s.ArmFreq != 2600 || *s.TempLimit != 80 {
		t.Errorf("got %+v", s)
	}
	for _, in := range []string{
		``,
		`{"arm_freq": 2600}{"arm_freq": 3000}`,
		`{"arm_freq": 2600, "force_turbo": 1}`,
		`{"arm_freq": "2600\nforce_turbo=1"}`,
		`{"arm_freq": 2600.5}`,
		`{"arm_freq": 2600` + strings.Repeat(" ", maxInput) + `}`,
	} {
		var s pifirmware.Settings
		err := readInput(strings.NewReader(in), &s)
		var r *pifirmware.RefusedError
		if !errors.As(err, &r) {
			t.Errorf("readInput(%.40q) = %v, want refused", in, err)
		}
	}
}

func TestRunOnlyAllowedCommands(t *testing.T) {
	if err := run(t.Context(), "sh", "-c", "true"); err == nil {
		t.Error("run(sh) = nil, want refused")
	}
}
