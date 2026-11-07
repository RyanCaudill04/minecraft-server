package pifirmware

import (
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func defaultFan() *FanCurve {
	f := Limits.FanDefault
	return &f
}

func TestValidate(t *testing.T) {
	ok := []Settings{
		{},
		{ArmFreq: new(1500)},
		{ArmFreq: new(3000), OverVoltageDelta: new(50000), TempLimit: new(60)},
		{TempLimit: new(85), Fan: defaultFan()},
	}
	for _, s := range ok {
		if err := s.Validate(); err != nil {
			t.Errorf("Validate(%+v) = %v, want nil", s, err)
		}
	}

	bad := map[string]Settings{
		"clock too low":    {ArmFreq: new(1499)},
		"clock too high":   {ArmFreq: new(3001)},
		"negative volts":   {OverVoltageDelta: new(-1)},
		"too many volts":   {OverVoltageDelta: new(50001)},
		"limit too high":   {TempLimit: new(86)},
		"limit too low":    {TempLimit: new(59)},
		"fan temp low":     {Fan: &FanCurve{{29000, 5000, 75}, {60000, 5000, 125}, {67500, 5000, 175}, {75000, 5000, 250}}},
		"fan temp high":    {Fan: &FanCurve{{50000, 5000, 75}, {60000, 5000, 125}, {67500, 5000, 175}, {80001, 5000, 250}}},
		"fan hyst":         {Fan: &FanCurve{{50000, 500, 75}, {60000, 5000, 125}, {67500, 5000, 175}, {75000, 5000, 250}}},
		"fan speed":        {Fan: &FanCurve{{50000, 5000, 75}, {60000, 5000, 125}, {67500, 5000, 175}, {75000, 5000, 256}}},
		"fan not rising":   {Fan: &FanCurve{{50000, 5000, 75}, {50000, 5000, 125}, {67500, 5000, 175}, {75000, 5000, 250}}},
		"fan slows":        {Fan: &FanCurve{{50000, 5000, 175}, {60000, 5000, 125}, {67500, 5000, 175}, {75000, 5000, 250}}},
		"fan off when hot": {Fan: &FanCurve{{50000, 5000, 0}, {60000, 5000, 0}, {67500, 5000, 0}, {75000, 5000, 0}}},
	}
	for name, s := range bad {
		if err := s.Validate(); err == nil {
			t.Errorf("%s: Validate(%+v) = nil, want an error", name, s)
		}
	}
}

// A request can only carry numbers: text, fractions and unknown keys
// don't decode into Settings at all.
func TestSettingsJSONIsNumbersOnly(t *testing.T) {
	for _, body := range []string{
		`{"arm_freq": "2400\nforce_turbo=1"}`,
		`{"arm_freq": 2400.5}`,
		`{"temp_limit": "80"}`,
		`{"fan": [{"temp": "50000,fan_temp0_speed=0"}]}`,
	} {
		var s Settings
		if err := json.Unmarshal([]byte(body), &s); err == nil {
			t.Errorf("%s decoded to %+v, want an error", body, s)
		}
	}
}

const stock = `# For more options and information see
# http://rptl.io/configtxt
dtparam=audio=on
camera_auto_detect=1

[cm5]
dtoverlay=dwc2,dr_mode=host

[all]
arm_64bit=1
`

func TestBlockRoundTrip(t *testing.T) {
	for _, s := range []Settings{
		{},
		{ArmFreq: new(2600), OverVoltageDelta: new(25000), TempLimit: new(80), Fan: defaultFan()},
	} {
		out, err := Splice([]byte(stock), Block(s))
		if err != nil {
			t.Fatal(err)
		}
		if !strings.HasPrefix(string(out), stock) {
			t.Errorf("the rest of config.txt changed:\n%s", out)
		}
		got, found, err := ParseBlock(out)
		if err != nil || !found {
			t.Fatalf("ParseBlock = found %v, %v", found, err)
		}
		if !reflect.DeepEqual(got, s) {
			t.Errorf("ParseBlock = %+v, want %+v", got, s)
		}
	}
}

func TestBlockText(t *testing.T) {
	s := Settings{ArmFreq: new(2600), Fan: defaultFan()}
	want := `# BEGIN admin
# Written by pi-firmware from admin. Change these from admin, not by hand.
[all]
arm_freq=2600
dtparam=fan_temp0=50000,fan_temp0_hyst=5000,fan_temp0_speed=75
dtparam=fan_temp1=60000,fan_temp1_hyst=5000,fan_temp1_speed=125
dtparam=fan_temp2=67500,fan_temp2_hyst=5000,fan_temp2_speed=175
dtparam=fan_temp3=75000,fan_temp3_hyst=5000,fan_temp3_speed=250
force_turbo=0
kernel_watchdog_timeout=60
# END admin
`
	if got := string(Block(s)); got != want {
		t.Errorf("Block =\n%s\nwant\n%s", got, want)
	}
}

func TestSpliceReplacesAndRemoves(t *testing.T) {
	first, err := Splice([]byte(stock), Block(Settings{ArmFreq: new(2600)}))
	if err != nil {
		t.Fatal(err)
	}
	second, err := Splice(first, Block(Settings{TempLimit: new(75)}))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Count(string(second), beginMarker) != 1 || strings.Contains(string(second), "arm_freq") {
		t.Errorf("replacing the block left:\n%s", second)
	}
	removed, err := Splice(second, nil)
	if err != nil {
		t.Fatal(err)
	}
	if string(removed) != stock {
		t.Errorf("removing the block left:\n%q\nwant\n%q", removed, stock)
	}
	// No block and nothing to write: unchanged.
	if same, _ := Splice([]byte(stock), nil); string(same) != stock {
		t.Errorf("Splice(stock, nil) = %q", same)
	}
	// A file without a final newline still gets a clean block.
	out, _ := Splice([]byte("arm_64bit=1"), Block(Settings{ArmFreq: new(2600)}))
	if !strings.HasPrefix(string(out), "arm_64bit=1\n\n# BEGIN admin\n") {
		t.Errorf("Splice without a final newline = %q", out)
	}
}

func TestSpliceRefusesOddFiles(t *testing.T) {
	block := Block(Settings{ArmFreq: new(2600)})
	cases := map[string]string{
		"two blocks":     stock + string(block) + string(block),
		"no end":         stock + "# BEGIN admin\n[all]\n",
		"end first":      stock + "# END admin\n# BEGIN admin\n",
		"settings after": stock + string(block) + "arm_freq=3000\n",
		"filter after":   stock + string(block) + "[pi4]\n",
	}
	for name, config := range cases {
		if _, err := Splice([]byte(config), block); err == nil {
			t.Errorf("%s: Splice = nil error", name)
		}
	}
	// Comments and blank lines after the block are fine.
	if _, err := Splice([]byte(stock+string(block)+"\n# a note\n"), block); err != nil {
		t.Errorf("comment after the block: %v", err)
	}
}

// Anything in the block that Block wouldn't have written is refused, so
// a hand edit (or an injected line) is never kept or copied on.
func TestParseBlockRefusesHandEdits(t *testing.T) {
	// Each body is otherwise a valid block, with the two lines every block
	// has.
	wrap := func(body string) []byte {
		return []byte(stock + "# BEGIN admin\n[all]\n" + body + "force_turbo=0\nkernel_watchdog_timeout=60\n# END admin\n")
	}
	for name, body := range map[string]string{
		"other key":      "force_turbo=1\n",
		"filter":         "arm_freq=2600\n[pi4]\narm_freq=2000\n",
		"no [all]":       "",
		"not a number":   "arm_freq=fast\n",
		"plus sign":      "arm_freq=+2600\n",
		"spaces":         "arm_freq= 2600\n",
		"duplicate":      "arm_freq=2600\narm_freq=2700\n",
		"out of range":   "arm_freq=4000\n",
		"extra dtparam":  "dtparam=audio=off\n",
		"dtparam suffix": "dtparam=fan_temp0=50000,fan_temp0_hyst=5000,fan_temp0_speed=75,audio=off\n",
		"dtparam spaces": "dtparam=fan_temp0= 50000,fan_temp0_hyst=5000,fan_temp0_speed=75\n",
		"wrong step":     "dtparam=fan_temp1=50000,fan_temp1_hyst=5000,fan_temp1_speed=75\n",
		"mixed steps":    "dtparam=fan_temp0=50000,fan_temp1_hyst=5000,fan_temp0_speed=75\n",
		"partial fan":    "dtparam=fan_temp0=50000,fan_temp0_hyst=5000,fan_temp0_speed=75\n",
		"other watchdog": "kernel_watchdog_timeout=5\n",
		"no equals":      "arm_freq\n",
	} {
		b := wrap(body)
		if name == "no [all]" {
			b = []byte(stock + "# BEGIN admin\narm_freq=2600\n# END admin\n")
		}
		if _, _, err := ParseBlock(b); err == nil {
			t.Errorf("%s: ParseBlock = nil error", name)
		} else if name != "no [all]" && !errors.Is(err, ErrHandEdited) {
			t.Errorf("%s: ParseBlock = %v, want ErrHandEdited", name, err)
		}
	}
	for name, b := range map[string]string{
		"no force_turbo": stock + "# BEGIN admin\n[all]\nkernel_watchdog_timeout=60\n# END admin\n",
		"no watchdog":    stock + "# BEGIN admin\n[all]\nforce_turbo=0\n# END admin\n",
	} {
		if _, _, err := ParseBlock([]byte(b)); !errors.Is(err, ErrHandEdited) {
			t.Errorf("%s: ParseBlock = %v, want ErrHandEdited", name, err)
		}
	}
	if _, found, err := ParseBlock([]byte(stock)); found || err != nil {
		t.Errorf("ParseBlock(no block) = found %v, %v", found, err)
	}
}
