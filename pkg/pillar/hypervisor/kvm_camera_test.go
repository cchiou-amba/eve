// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package hypervisor

import (
	"bufio"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/lf-edge/eve/pkg/pillar/types"
	uuid "github.com/satori/go.uuid"
)

// These tests replace package variables (the admin socket, the lease directory, the camera
// window nodes), so none of them runs in parallel.

const cameraTestUUID = "3c4af9b0-7a70-4164-8bf3-122cd468c7af"

// fakeAdmin is a stand-in for amba-virt-server's admin socket. It answers one request per
// connection with whatever handler returns.
type fakeAdmin struct {
	mu       sync.Mutex
	requests []string
}

func (f *fakeAdmin) seen() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.requests...)
}

func startFakeAdmin(t *testing.T, handler func(req string) string) *fakeAdmin {
	t.Helper()
	// A short path: unix socket paths are limited to about a hundred bytes.
	dir, err := os.MkdirTemp("/tmp", "adm")
	if err != nil {
		t.Fatalf("MkdirTemp: %v", err)
	}
	sock := filepath.Join(dir, "s")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	f := &fakeAdmin{}
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			c, err := l.Accept()
			if err != nil {
				return
			}
			line, _ := bufio.NewReader(c).ReadString('\n')
			req := strings.TrimSpace(line)
			f.mu.Lock()
			f.requests = append(f.requests, req)
			f.mu.Unlock()
			_, _ = c.Write([]byte(handler(req)))
			c.Close()
		}
	}()
	old := ambaVirtAdminSock
	ambaVirtAdminSock = sock
	t.Cleanup(func() {
		ambaVirtAdminSock = old
		l.Close()
		<-done
		os.RemoveAll(dir)
	})
	return f
}

// cameraTestEnv points the lease records at a temporary directory and the camera window nodes at
// distinct character devices, like the real nodes. The fake server's Cavalry slice is /dev/null.
func cameraTestEnv(t *testing.T) {
	t.Helper()
	dir := t.TempDir()
	oldDir := domainStateDir
	oldPaths := cameraWindowPaths
	domainStateDir = func(domainName string) string { return filepath.Join(dir, domainName) }
	cameraWindowPaths = map[string]string{
		"dsp": "/dev/zero", "bsb": "/dev/full", "dec": "/dev/random", "ovl": "/dev/urandom",
	}
	t.Cleanup(func() {
		domainStateDir = oldDir
		cameraWindowPaths = oldPaths
	})
}

// cameraServer answers like amba-virt-server: it takes the request's index and intent and returns
// camera=1 with the given window sizes when the intent is on.
func cameraServer(onGrant string) func(req string) string {
	return func(req string) string {
		f := strings.Fields(req)
		switch f[0] {
		case "RESERVE_SLICE_AT", "RESERVE_SLICE_MIN":
			index := f[3]
			if f[0] == "RESERVE_SLICE_MIN" {
				index = "1"
			}
			base := "OK index=" + index + " path=/dev/null epoch=7"
			if len(f) == 6 && f[5] == "on" {
				return base + onGrant + "\n"
			}
			return base + " camera=0\n"
		case "RESERVE_LOW":
			return "OK index=1 path=/dev/null epoch=8\n"
		}
		return "ERR UNKNOWN_CMD\n"
	}
}

func cameraDomain(t *testing.T, label string, cbattr map[string]string) (types.DomainConfig,
	types.AssignableAdapters, []types.DiskStatus) {
	t.Helper()
	diskConfigs, diskStatuses := qemuDisks()
	config, aa := domainConfigAndAssignableAdapters(diskConfigs)
	config.VirtualizationMode = types.HVM
	config.IoAdapterList = append(config.IoAdapterList, types.IoAdapter{Type: types.IoOther, Name: label})
	aa.IoBundleList = append(aa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: label,
		Phylabel:        label,
		Logicallabel:    label,
		Cbattr:          cbattr,
		UsedByUUID:      config.UUIDandVersion.UUID,
	})
	return config, aa, diskStatuses
}

func renderCameraConfig(t *testing.T, config types.DomainConfig, aa types.AssignableAdapters,
	diskStatuses []types.DiskStatus) (string, error) {
	t.Helper()
	conf, err := os.CreateTemp("/tmp", "config")
	if err != nil {
		t.Fatalf("can't create config file: %v", err)
	}
	defer os.Remove(conf.Name())
	cerr := kvmArm.CreateDomConfig(DefaultDomainName, config, types.DomainStatus{},
		diskStatuses, &aa, nil, swtpmCtrlSock, conf)
	if cerr != nil {
		return "", cerr
	}
	out, err := os.ReadFile(conf.Name())
	if err != nil {
		t.Fatalf("reading config failed: %v", err)
	}
	return string(out), nil
}

func countObject(cfg, id string) int {
	return strings.Count(cfg, `[object "`+id+`"]`)
}

func TestCameraTicketLabels(t *testing.T) {
	long := strings.Repeat("a", 48)
	for label, want := range map[string]bool{
		"amba_shm0": true, "amba_shm7": true, "a": true, "A-b.c_9": true, long: true,
		"": false, "a b": false, "a/b": false, "a\nb": false, "amba_sh\u00e9": false,
		long + "a": false,
	} {
		if got := validTicketLabel(label); got != want {
			t.Errorf("validTicketLabel(%q) = %v, want %v", label, got, want)
		}
	}
}

func TestCameraReserveCommand(t *testing.T) {
	const bytes = uint64(1 << 30)
	for _, tc := range []struct{ label, intent, want string }{
		{"amba_shm0", "on", "RESERVE_SLICE_AT " + cameraTestUUID + " 1073741824 0 amba_shm0 on"},
		{"amba_shm0", "absent", "RESERVE_SLICE_AT " + cameraTestUUID + " 1073741824 0 amba_shm0 absent"},
		{"amba_shm1", "off", "RESERVE_SLICE_MIN " + cameraTestUUID + " 1073741824 1 amba_shm1 off"},
		{"amba_shm2", "absent", "RESERVE_SLICE_MIN " + cameraTestUUID + " 1073741824 1 amba_shm2 absent"},
		// The digit is a label, not an index: amba_shm3 must not pin slice 3.
		{"amba_shm3", "on", "RESERVE_SLICE_MIN " + cameraTestUUID + " 1073741824 1 amba_shm3 on"},
		{"amba_shm7", "on", "RESERVE_SLICE_MIN " + cameraTestUUID + " 1073741824 1 amba_shm7 on"},
		{"amba_shm", "on", "RESERVE_SLICE_MIN " + cameraTestUUID + " 1073741824 1 amba_shm on"},
		{"ticket_9.x-y", "on", "RESERVE_SLICE_MIN " + cameraTestUUID + " 1073741824 1 ticket_9.x-y on"},
	} {
		got, err := reserveSliceCommand(cameraTestUUID, bytes, tc.label, tc.intent)
		if err != nil {
			t.Errorf("%s/%s: unexpected error %v", tc.label, tc.intent, err)
			continue
		}
		if got != tc.want {
			t.Errorf("%s/%s: command %q, want %q", tc.label, tc.intent, got, tc.want)
		}
	}
	for _, tc := range []struct{ label, intent string }{
		{"", "on"}, {"a b", "on"}, {"a/b", "on"}, {strings.Repeat("a", 49), "on"}, {"amba_sh\u00e9", "on"},
		{"amba_shm0", ""}, {"amba_shm0", "ON"}, {"amba_shm0", "maybe"},
	} {
		if cmd, err := reserveSliceCommand(cameraTestUUID, bytes, tc.label, tc.intent); err == nil {
			t.Errorf("%q/%q: got command %q, want an error", tc.label, tc.intent, cmd)
		}
	}

	// A bad label or intent is refused before the server is asked anything.
	f := startFakeAdmin(t, func(string) string { return "ERR INVAL\n" })
	if _, err := reserveSliceLease(cameraTestUUID, bytes, "bad/label", "on"); err == nil {
		t.Errorf("reserveSliceLease accepted a bad ticket label")
	}
	if _, err := reserveSliceLease(cameraTestUUID, bytes, "amba_shm0", "maybe"); err == nil {
		t.Errorf("reserveSliceLease accepted a bad intent")
	}
	if n := len(f.seen()); n != 0 {
		t.Errorf("the server saw %d requests for refused reserves, want 0", n)
	}
}

func TestCameraGrantParse(t *testing.T) {
	const base = "OK index=%d path=/dev/amba_virt_shm%d epoch=3"
	grant := func(index int, tail string) string {
		r := strings.NewReplacer("%d", string(rune('0'+index)))
		return r.Replace(base) + tail
	}
	keys := func(w []cameraWindow) string {
		var s []string
		for _, x := range w {
			s = append(s, x.Key)
		}
		return strings.Join(s, ",")
	}
	for _, tc := range []struct {
		name  string
		index int
		resp  string
		want  string // keys, or "" for none
		fail  string // error substring, or "" for success
	}{
		{"off", 1, grant(1, " camera=0"), "", ""},
		{"off ignores an unknown token", 1, grant(1, " camera=0 future=5"), "", ""},
		{"consumer", 1, grant(1, " camera=1 dsp=1073741824 bsb=16777216"), "dsp,bsb", ""},
		{"tokens in any order give dsp then bsb", 1, grant(1, " camera=1 bsb=16777216 dsp=1073741824"), "dsp,bsb", ""},
		{"primary", 0, grant(0, " camera=1 dsp=1073741824 bsb=16777216 dec=8388608 ovl=8388608"), "dsp,bsb,dec,ovl", ""},
		{"primary with overlay only", 0, grant(0, " camera=1 dsp=1073741824 bsb=16777216 ovl=8388608"), "dsp,bsb,ovl", ""},
		{"smallest size", 1, grant(1, " camera=1 dsp=4096 bsb=4096"), "dsp,bsb", ""},
		{"no camera verdict", 1, grant(1, ""), "", "no camera verdict"},
		{"camera=2", 1, grant(1, " camera=2"), "", "camera="},
		{"camera empty", 1, grant(1, " camera="), "", "camera="},
		{"camera off with a size", 1, grant(1, " camera=0 dsp=1073741824"), "", "without the camera"},
		{"camera on without bsb", 1, grant(1, " camera=1 dsp=1073741824"), "", "both dsp and bsb"},
		{"camera on without dsp", 1, grant(1, " camera=1 bsb=16777216"), "", "both dsp and bsb"},
		{"zero size", 1, grant(1, " camera=1 dsp=0 bsb=16777216"), "", "power of two"},
		{"not a power of two", 1, grant(1, " camera=1 dsp=3000 bsb=16777216"), "", "power of two"},
		{"over the maximum", 1, grant(1, " camera=1 dsp=137438953472 bsb=16777216"), "", "power of two"},
		{"not a number", 1, grant(1, " camera=1 dsp=abc bsb=16777216"), "", "dsp="},
		{"decode for a consumer", 1, grant(1, " camera=1 dsp=1073741824 bsb=16777216 dec=8388608"), "", "decode or overlay"},
		{"overlay for a consumer", 1, grant(1, " camera=1 dsp=1073741824 bsb=16777216 ovl=8388608"), "", "decode or overlay"},
		{"duplicate window", 1, grant(1, " camera=1 dsp=1073741824 dsp=1073741824 bsb=16777216"), "", "duplicate dsp"},
		{"duplicate camera", 1, grant(1, " camera=1 camera=1 dsp=1073741824 bsb=16777216"), "", "duplicate camera"},
		{"server error", 1, "ERR EXHAUSTED\n", "", "ERR EXHAUSTED"},
		{"empty", 1, "", "", "slice reserve"},
	} {
		got, err := parseCameraGrant(tc.resp, uint32(tc.index))
		if tc.fail != "" {
			if err == nil {
				t.Errorf("%s: got windows %v, want an error containing %q", tc.name, keys(got), tc.fail)
			} else if tc.name == "empty" {
				// an empty reply has no OK
			} else if !strings.Contains(err.Error(), tc.fail) {
				t.Errorf("%s: error %q, want it to contain %q", tc.name, err, tc.fail)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if keys(got) != tc.want {
			t.Errorf("%s: windows %q, want %q", tc.name, keys(got), tc.want)
		}
	}
	// Sizes are carried through.
	w, err := parseCameraGrant(grant(0, " camera=1 dsp=1073741824 bsb=16777216 dec=8388608 ovl=4194304"), 0)
	if err != nil || len(w) != 4 || w[0].Size != 1<<30 || w[1].Size != 16<<20 || w[2].Size != 8<<20 || w[3].Size != 4<<20 {
		t.Errorf("sizes not carried: %+v, %v", w, err)
	}
}

func TestCameraIntentFromBundle(t *testing.T) {
	for _, tc := range []struct {
		name   string
		cbattr map[string]string
		want   string // intent, or "" for "no window"
		fail   bool
	}{
		{"on", map[string]string{"shmassign": "server", "shmsize": "1G", "camera": "on"}, "on", false},
		{"off", map[string]string{"shmassign": "server", "shmsize": "1G", "camera": "off"}, "off", false},
		{"absent", map[string]string{"shmassign": "server", "shmsize": "1G"}, "absent", false},
		{"empty value", map[string]string{"shmassign": "server", "shmsize": "1G", "camera": ""}, "absent", false},
		{"maybe", map[string]string{"shmassign": "server", "shmsize": "1G", "camera": "maybe"}, "", true},
		{"upper case", map[string]string{"shmassign": "server", "shmsize": "1G", "camera": "ON"}, "", true},
		{"on without server assignment", map[string]string{"shmpath": "/dev/null", "shmsize": "1G", "camera": "on"}, "", true},
		{"legacy low marker ignores it", map[string]string{"shmassign": "server", "pool": "low", "camera": "on"}, "", false},
	} {
		w, err := ivshmemWindowFromBundle(types.IoBundle{
			Type: types.IoOther, Logicallabel: "amba_shm1", Cbattr: tc.cbattr,
		})
		if tc.fail {
			if err == nil {
				t.Errorf("%s: want an error", tc.name)
			}
			continue
		}
		if err != nil {
			t.Errorf("%s: unexpected error %v", tc.name, err)
			continue
		}
		if tc.want == "" {
			if w != nil {
				t.Errorf("%s: got a window %+v, want none", tc.name, *w)
			}
			continue
		}
		if w == nil || w.cameraIntent != tc.want {
			t.Errorf("%s: window %+v, want intent %q", tc.name, w, tc.want)
		}
	}
}

func TestCameraCreateDomConfigOnIndexOne(t *testing.T) {
	cameraTestEnv(t)
	f := startFakeAdmin(t, cameraServer(" camera=1 dsp=1073741824 bsb=16777216"))
	config, aa, disks := cameraDomain(t, "amba_shm1", map[string]string{
		"shmassign": "server", "shmsize": "1G", "camera": "on"})

	cfg, err := renderCameraConfig(t, config, aa, disks)
	if err != nil {
		t.Fatalf("CreateDomConfig failed: %v", err)
	}
	for _, id := range []string{"amba_shm1", "amba_iav_dsp", "amba_iav_bsb", "iav-slice1"} {
		if countObject(cfg, id) != 1 {
			t.Errorf("config has %d objects %q, want 1:\n%s", countObject(cfg, id), id, cfg)
		}
	}
	for _, id := range []string{"amba_iav_dec", "amba_iav_ovl"} {
		if countObject(cfg, id) != 0 {
			t.Errorf("a consumer got %s:\n%s", id, cfg)
		}
	}
	for _, want := range []string{`size = "1073741824"`, `size = "16777216"`,
		`mem-path = "/dev/zero"`, `mem-path = "/dev/full"`} {
		if !strings.Contains(cfg, want) {
			t.Errorf("config is missing %q", want)
		}
	}
	var sawReserve bool
	for _, r := range f.seen() {
		if strings.HasPrefix(r, "RESERVE_SLICE_MIN ") {
			sawReserve = true
			if !strings.HasSuffix(r, " 1 amba_shm1 on") {
				t.Errorf("reserve request %q, want ticket amba_shm1 with intent on", r)
			}
		}
	}
	if !sawReserve {
		t.Errorf("the server was never asked for a slice: %v", f.seen())
	}
}

func TestCameraCreateDomConfigOnIndexZero(t *testing.T) {
	cameraTestEnv(t)
	f := startFakeAdmin(t, cameraServer(" camera=1 dsp=1073741824 bsb=16777216 dec=8388608 ovl=8388608"))
	config, aa, disks := cameraDomain(t, "amba_shm0", map[string]string{
		"shmassign": "server", "shmsize": "1G", "camera": "on"})

	cfg, err := renderCameraConfig(t, config, aa, disks)
	if err != nil {
		t.Fatalf("CreateDomConfig failed: %v", err)
	}
	for _, id := range []string{"amba_iav_dsp", "amba_iav_bsb", "amba_iav_dec", "amba_iav_ovl"} {
		if countObject(cfg, id) != 1 {
			t.Errorf("config has %d objects %q, want 1:\n%s", countObject(cfg, id), id, cfg)
		}
	}
	for _, want := range []string{`size = "8388608"`} {
		if strings.Count(cfg, want) != 2 {
			t.Errorf("config has %d of %q, want 2 (decode and overlay)", strings.Count(cfg, want), want)
		}
	}
	var at bool
	for _, r := range f.seen() {
		if strings.HasPrefix(r, "RESERVE_SLICE_AT ") && strings.HasSuffix(r, " 0 amba_shm0 on") {
			at = true
		}
	}
	if !at {
		t.Errorf("no RESERVE_SLICE_AT ... 0 amba_shm0 on in %v", f.seen())
	}
}

func TestCameraCreateDomConfigOff(t *testing.T) {
	cameraTestEnv(t)
	f := startFakeAdmin(t, cameraServer(" camera=1 dsp=1073741824 bsb=16777216"))
	// No camera key: intent absent, so the server says camera=0.
	config, aa, disks := cameraDomain(t, "amba_shm2", map[string]string{
		"shmassign": "server", "shmsize": "1G"})

	cfg, err := renderCameraConfig(t, config, aa, disks)
	if err != nil {
		t.Fatalf("CreateDomConfig failed: %v", err)
	}
	for _, id := range []string{"amba_iav_dsp", "amba_iav_bsb", "amba_iav_dec", "amba_iav_ovl"} {
		if countObject(cfg, id) != 0 {
			t.Errorf("a camera-off ticket got %s:\n%s", id, cfg)
		}
	}
	if countObject(cfg, "amba_shm2") != 1 || countObject(cfg, "iav-slice1") != 1 {
		t.Errorf("the Cavalry slice and the private slice must still be present:\n%s", cfg)
	}
	for _, r := range f.seen() {
		if strings.HasPrefix(r, "RESERVE_SLICE_MIN ") && !strings.HasSuffix(r, " 1 amba_shm2 absent") {
			t.Errorf("reserve request %q, want intent absent", r)
		}
	}
}

func TestCameraServerUnreachable(t *testing.T) {
	cameraTestEnv(t)
	old := ambaVirtAdminSock
	ambaVirtAdminSock = filepath.Join(t.TempDir(), "nobody-listens")
	defer func() { ambaVirtAdminSock = old }()
	config, aa, disks := cameraDomain(t, "amba_shm1", map[string]string{
		"shmassign": "server", "shmsize": "1G", "camera": "on"})

	if _, err := renderCameraConfig(t, config, aa, disks); err == nil {
		t.Fatalf("CreateDomConfig succeeded with no server; it must not guess a camera verdict")
	} else if !strings.Contains(err.Error(), "slice reserve") {
		t.Errorf("error %q does not name the slice reserve", err)
	}
}

func TestCameraModelWindowConflict(t *testing.T) {
	cameraTestEnv(t)
	startFakeAdmin(t, cameraServer(" camera=1 dsp=1073741824 bsb=16777216"))
	config, aa, disks := cameraDomain(t, "amba_shm1", map[string]string{
		"shmassign": "server", "shmsize": "1G", "camera": "on"})
	// A legacy path adapter on the same node the server is about to attach.
	config.IoAdapterList = append(config.IoAdapterList, types.IoAdapter{Type: types.IoOther, Name: "amba_iav_dsp"})
	aa.IoBundleList = append(aa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "amba_iav_dsp",
		Phylabel:        "amba_iav_dsp",
		Logicallabel:    "amba_iav_dsp",
		Cbattr:          map[string]string{"shmpath": "/dev/zero", "shmsize": "1G"},
		UsedByUUID:      config.UUIDandVersion.UUID,
	})

	_, err := renderCameraConfig(t, config, aa, disks)
	if err == nil {
		t.Fatalf("CreateDomConfig accepted two windows on one node")
	}
	if !strings.Contains(err.Error(), "both backed by") {
		t.Errorf("error %q does not say two windows share a node", err)
	}
}

func TestCameraRepeatReserveFollowsVerdict(t *testing.T) {
	cameraTestEnv(t)
	var mu sync.Mutex
	camera := true
	// The server's verdict comes from its own override, whatever the model's intent says.
	startFakeAdmin(t, func(req string) string {
		f := strings.Fields(req)
		if f[0] == "RESERVE_LOW" {
			return "OK index=1 path=/dev/null epoch=8\n"
		}
		mu.Lock()
		defer mu.Unlock()
		base := "OK index=1 path=/dev/null epoch=7"
		if camera {
			return base + " camera=1 dsp=1073741824 bsb=16777216\n"
		}
		return base + " camera=0\n"
	})
	config, aa, disks := cameraDomain(t, "amba_shm1", map[string]string{
		"shmassign": "server", "shmsize": "1G", "camera": "on"})

	cfg, err := renderCameraConfig(t, config, aa, disks)
	if err != nil {
		t.Fatalf("first CreateDomConfig failed: %v", err)
	}
	if countObject(cfg, "amba_iav_dsp") != 1 {
		t.Fatalf("first config has no camera window:\n%s", cfg)
	}

	// The operator forced the camera off for this ticket; the guest is started again with the
	// same model and the same lease.
	mu.Lock()
	camera = false
	mu.Unlock()
	cfg, err = renderCameraConfig(t, config, aa, disks)
	if err != nil {
		t.Fatalf("second CreateDomConfig failed: %v", err)
	}
	if countObject(cfg, "amba_iav_dsp") != 0 || countObject(cfg, "amba_iav_bsb") != 0 {
		t.Errorf("the second config still has camera windows:\n%s", cfg)
	}
	if countObject(cfg, "amba_shm1") != 1 {
		t.Errorf("the second config lost the Cavalry slice:\n%s", cfg)
	}
}

func TestCameraEstimatorIgnoresCameraWindows(t *testing.T) {
	cameraTestEnv(t)
	f := startFakeAdmin(t, cameraServer(" camera=1 dsp=1073741824 bsb=16777216"))
	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("NewV4: %v", err)
	}
	adapters := []types.IoAdapter{{Type: types.IoOther, Name: "amba_shm4"}}
	overhead := func(camera string) int64 {
		cbattr := map[string]string{"shmassign": "server", "shmsize": "1G"}
		if camera != "" {
			cbattr["camera"] = camera
		}
		aa := types.AssignableAdapters{
			Initialized: true,
			IoBundleList: []types.IoBundle{{
				Type:            types.IoOther,
				AssignmentGroup: "amba_shm4",
				Phylabel:        "amba_shm4",
				Logicallabel:    "amba_shm4",
				Cbattr:          cbattr,
				UsedByUUID:      id,
			}},
		}
		got, err := ivshmemVMMOverhead(DefaultDomainName, &aa, adapters, id)
		if err != nil {
			t.Fatalf("ivshmemVMMOverhead(camera=%q): %v", camera, err)
		}
		return got
	}
	on, off, absent := overhead("on"), overhead("off"), overhead("")
	if on != off || off != absent {
		t.Errorf("overhead depends on the camera intent: on=%d off=%d absent=%d", on, off, absent)
	}
	// The ticket window is backed by a regular file until it is leased, so it is charged in full;
	// the camera windows add nothing.
	if want := int64(1 << 30); on != want {
		t.Errorf("overhead %d, want %d (the ticket window only)", on, want)
	}
	if n := len(f.seen()); n != 0 {
		t.Errorf("the estimator asked the server %d times, want 0", n)
	}
}
