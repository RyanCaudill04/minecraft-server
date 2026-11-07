package sysinfo

import (
	"reflect"
	"testing"
)

func TestReadThrottled(t *testing.T) {
	file := func(body string) string {
		return writeFiles(t, map[string]string{"get_throttled": body}) + "/get_throttled"
	}
	for _, tc := range []struct {
		body string
		want Throttle
	}{
		{"0\n", Throttle{}},
		// Too hot earlier, fine now: soft limit and throttling since boot.
		{"0xc0000\n", Throttle{SinceBoot: ThrottleFlags{Throttled: true, SoftTempLimit: true}, Raw: 0xc0000}},
		{"f000f", Throttle{
			Now:       ThrottleFlags{true, true, true, true},
			SinceBoot: ThrottleFlags{true, true, true, true},
			Raw:       0xf000f,
		}},
	} {
		got, err := ReadThrottled(file(tc.body))
		if err != nil || got != tc.want {
			t.Errorf("ReadThrottled(%q) = %+v, %v; want %+v", tc.body, got, err, tc.want)
		}
	}
	for _, body := range []string{"", "throttled", "1ffffffff"} {
		if v, err := ReadThrottled(file(body)); err == nil {
			t.Errorf("ReadThrottled(%q) = %+v, want an error", body, v)
		}
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
}
