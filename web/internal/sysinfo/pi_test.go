package sysinfo

import (
	"reflect"
	"strings"
	"testing"
)

func TestParseThrottled(t *testing.T) {
	for _, tc := range []struct {
		out  string
		want Throttle
	}{
		{"throttled=0x0\n", Throttle{}},
		// Too hot a moment ago, fine now.
		{"throttled=0xc0000\n", Throttle{Recent: ThrottleFlags{Throttled: true, SoftTempLimit: true}, Raw: 0xc0000}},
		{"throttled=f000f", Throttle{
			Now:    ThrottleFlags{true, true, true, true},
			Recent: ThrottleFlags{true, true, true, true},
			Raw:    0xf000f,
		}},
	} {
		got, err := ParseThrottled(tc.out)
		if err != nil || got != tc.want {
			t.Errorf("ParseThrottled(%q) = %+v, %v; want %+v", tc.out, got, err, tc.want)
		}
	}
	for _, out := range []string{"", "0x0", "throttled=", "throttled=hot", "throttled=0x1ffffffff", "error=1 error_msg=\"Command not registered\""} {
		if v, err := ParseThrottled(out); err == nil {
			t.Errorf("ParseThrottled(%q) = %+v, want an error", out, v)
		}
	}

	// A failing vcgencmd (not installed, or not in the video group) is an
	// error, not all-clear flags.
	r := &Reader{Root: t.TempDir(), Run: noVcgencmd}
	if v, err := r.readThrottle(); err == nil {
		t.Errorf("readThrottle without vcgencmd = %+v, want an error", v)
	}
}

func TestReadKHzAndFan(t *testing.T) {
	file := func(body string) string {
		return writeFiles(t, map[string]string{"f": body}) + "/f"
	}
	for _, body := range []string{"", "1.5GHz", "-1"} {
		if v, err := ReadKHz(file(body)); err == nil {
			t.Errorf("ReadKHz(%q) = %v, want an error", body, v)
		}
	}
	r := &Reader{Root: writeFiles(t, map[string]string{
		"sys/devices/platform/cooling_fan/hwmon/hwmon2/fan1_input": "fast\n",
	})}
	if v, err := r.readFan(); err == nil {
		t.Errorf("readFan of a bad file = %v, want an error", v)
	}
}

func TestReadConfigTxt(t *testing.T) {
	// Before bookworm, config.txt was in /boot. A config.txt that sets
	// nothing of interest is all defaults: empty, not missing.
	r := &Reader{Root: writeFiles(t, map[string]string{
		"boot/config.txt": "# nothing here\ndtparam=audio=on\ncamera_auto_detect=1\n[cm4]\notg_mode=1\n",
	})}
	got, err := r.readFirmware()
	if err != nil || got == nil || len(got) != 0 {
		t.Errorf("readFirmware of a default config.txt = %#v, %v; want empty", got, err)
	}

	// /boot/firmware wins when both are there.
	r = &Reader{Root: writeFiles(t, map[string]string{
		"boot/config.txt":          "arm_freq=1000\n",
		"boot/firmware/config.txt": "  arm_freq = 2000  \n[pi4]\n[all]\ntemp_limit=75\ninclude extra.txt\nnot a setting\n",
	})}
	got, err = r.readFirmware()
	want := []FirmwareSetting{{Key: "arm_freq", Value: "2000"}, {Key: "temp_limit", Value: "75"}}
	if err != nil || !reflect.DeepEqual(got, want) {
		t.Errorf("readFirmware = %+v, %v; want %+v", got, err, want)
	}

	if _, err := (&Reader{Root: t.TempDir()}).readFirmware(); err == nil {
		t.Error("readFirmware with no config.txt: want an error")
	}

	// A line too long to scan is an error, not a quietly shorter list.
	long := "arm_freq=2000\n#" + strings.Repeat("x", 70<<10) + "\n[pi5]\nover_voltage_delta=50000\n"
	path := writeFiles(t, map[string]string{"config.txt": long}) + "/config.txt"
	if v, err := ReadConfigTxt(path); err == nil {
		t.Errorf("ReadConfigTxt with a 70 KB line = %+v, want an error", v)
	}
}
