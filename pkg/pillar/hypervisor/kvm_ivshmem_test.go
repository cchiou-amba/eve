// Copyright (c) 2026 Zededa, Inc.
// SPDX-License-Identifier: Apache-2.0

package hypervisor

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/lf-edge/eve/pkg/pillar/types"
	uuid "github.com/satori/go.uuid"
)

func TestParseIvshmemSize(t *testing.T) {
	t.Parallel()

	valid := map[string]uint64{
		"4096": 4096,
		"16M":  16 << 20,
		"16m":  16 << 20,
		"256M": 256 << 20,
		"1G":   1 << 30,
		"64K":  64 << 10,
		" 8M ": 8 << 20,
	}
	for in, want := range valid {
		got, err := parseIvshmemSize(in)
		if err != nil {
			t.Errorf("parseIvshmemSize(%q) failed: %v", in, err)
			continue
		}
		if got != want {
			t.Errorf("parseIvshmemSize(%q) = %d, want %d", in, got, want)
		}
	}

	invalid := []string{"", "M", "abc", "0", "-1", "16MB", "1024G"}
	for _, in := range invalid {
		if got, err := parseIvshmemSize(in); err == nil {
			t.Errorf("parseIvshmemSize(%q) = %d, want error", in, got)
		}
	}
}

func TestIvshmemWindowFromBundle(t *testing.T) {
	t.Parallel()

	// Fully specified by the controller.
	w, err := ivshmemWindowFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "amba_shm",
		Logicallabel: "amba_shm",
		Cbattr: map[string]string{
			"shmpath": "/dev/shm/amba-virt",
			"shmsize": "16M",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w == nil {
		t.Fatal("expected a window for a bare IoOther bundle with cbattr")
	}
	if w.id != "amba_shm" || w.memPath != "/dev/shm/amba-virt" || w.size != 16<<20 {
		t.Errorf("got %+v, want id=amba_shm path=/dev/shm/amba-virt size=%d", *w, 16<<20)
	}

	// cbattr stripped by the controller: fall back to a derived path and the
	// default size.
	w, err = ivshmemWindowFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "amba_shm",
		Logicallabel: "amba_shm",
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w == nil {
		t.Fatal("expected a window when cbattr is absent")
	}
	if w.memPath != "/dev/shm/amba_shm" || w.size != ivshmemDefaultSize {
		t.Errorf("got %+v, want derived path and default size", *w)
	}

	// Bundles that carry a real resource are not markers.
	notMarkers := []types.IoBundle{
		{Type: types.IoOther, Phylabel: "amba_virt", Logicallabel: "amba_virt", Ifname: "/dev/amba_virt"},
		{Type: types.IoOther, Phylabel: "cavalry", Logicallabel: "cavalry", Ifname: "/dev/cavalry"},
		{Type: types.IoCom, Phylabel: "COM1", Logicallabel: "COM1", Serial: "/dev/ttyS0"},
		{Type: types.IoNetEth, Phylabel: "eth0", Logicallabel: "eth0", PciLong: "0000:f3:00.0"},
		{Type: types.IoUSBDevice, Phylabel: "USB1:1", UsbAddr: "1:1"},
		{Type: types.IoOther},
	}
	for _, ib := range notMarkers {
		w, err := ivshmemWindowFromBundle(ib)
		if err != nil {
			t.Errorf("unexpected error for %+v: %v", ib, err)
		}
		if w != nil {
			t.Errorf("bundle %+v wrongly treated as an ivshmem marker (%+v)", ib, *w)
		}
	}

	// A bad size in the model is an error, not a silent default.
	if _, err := ivshmemWindowFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Logicallabel: "amba_shm",
		Cbattr:       map[string]string{"shmsize": "not-a-size"},
	}); err == nil {
		t.Error("expected an error for an unparseable shmsize")
	}
}

func TestEnsureSharedMemoryFile(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()

	// A PCI BAR has to be a power of two.
	if err := ensureSharedMemoryFile(ivshmemWindow{
		id: "bad", memPath: filepath.Join(dir, "bad"), size: 384 << 20,
	}); err == nil {
		t.Error("expected a non-power-of-two size to be rejected")
	}
	if err := ensureSharedMemoryFile(ivshmemWindow{
		id: "tiny", memPath: filepath.Join(dir, "tiny"), size: 512,
	}); err == nil {
		t.Error("expected a sub-page size to be rejected")
	}

	// Creates and sizes the backing file.
	path := filepath.Join(dir, "amba-virt")
	w := ivshmemWindow{id: "amba_shm", memPath: path, size: 16 << 20}
	if err := ensureSharedMemoryFile(w); err != nil {
		t.Fatalf("ensureSharedMemoryFile failed: %v", err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("backing file missing: %v", err)
	}
	if st.Size() != 16<<20 {
		t.Errorf("backing file is %d bytes, want %d", st.Size(), 16<<20)
	}

	// Growing is allowed, shrinking is not: a container may already hold a
	// mapping of the larger region.
	w.size = 32 << 20
	if err := ensureSharedMemoryFile(w); err != nil {
		t.Fatalf("growing failed: %v", err)
	}
	if st, _ := os.Stat(path); st.Size() != 32<<20 {
		t.Errorf("after grow the file is %d bytes, want %d", st.Size(), 32<<20)
	}
	w.size = 16 << 20
	if err := ensureSharedMemoryFile(w); err != nil {
		t.Fatalf("re-declaring a smaller window failed: %v", err)
	}
	if st, _ := os.Stat(path); st.Size() != 32<<20 {
		t.Errorf("file shrank to %d bytes, want it left at %d", st.Size(), 32<<20)
	}
}

// The ivshmem stanza has to land before the vsock device, which CreateDomConfig
// keeps last on purpose so qemu assigns its PCI ID without conflicts.
func TestCreateDomConfigIvshmemPrecedesVsock(t *testing.T) {
	t.Parallel()

	conf, err := os.CreateTemp("/tmp", "config")
	if err != nil {
		t.Fatalf("can't create config file for a domain %v", err)
	}
	defer os.Remove(conf.Name())

	diskConfigs, diskStatuses := qemuDisks()
	config, aa := domainConfigAndAssignableAdapters(diskConfigs)
	config.VirtualizationMode = types.HVM

	shmPath := filepath.Join(t.TempDir(), "amba-virt")
	config.IoAdapterList = append(config.IoAdapterList, types.IoAdapter{
		Type: types.IoOther,
		Name: "amba_shm",
	})
	aa.IoBundleList = append(aa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "amba_shm",
		Phylabel:        "amba_shm",
		Logicallabel:    "amba_shm",
		Cbattr:          map[string]string{"shmpath": shmPath, "shmsize": "16M"},
		UsedByUUID:      config.UUIDandVersion.UUID,
	})

	if err := kvmArm.CreateDomConfig(DefaultDomainName, config, types.DomainStatus{},
		diskStatuses, &aa, nil, swtpmCtrlSock, conf); err != nil {
		t.Fatalf("CreateDomConfig failed %v", err)
	}
	defer os.Truncate(conf.Name(), 0)

	result, err := os.ReadFile(conf.Name())
	if err != nil {
		t.Fatalf("reading conf file failed %v", err)
	}
	got := string(result)

	for _, want := range []string{
		`[object "amba_shm"]`,
		`qom-type = "memory-backend-file"`,
		`mem-path = "` + shmPath + `"`,
		`size = "16777216"`,
		`share = "on"`,
		`[device "amba_shm-dev"]`,
		`driver = "ivshmem-plain"`,
		`memdev = "amba_shm"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated config is missing %q:\n%s", want, got)
		}
	}

	ivshmem := strings.Index(got, `[device "amba_shm-dev"]`)
	vsock := strings.Index(got, `[device "eve-vsock0"]`)
	if ivshmem < 0 || vsock < 0 {
		t.Fatalf("expected both ivshmem and vsock devices:\n%s", got)
	}
	if ivshmem > vsock {
		t.Errorf("ivshmem device must be emitted before vsock, got ivshmem at %d and vsock at %d",
			ivshmem, vsock)
	}

	if st, err := os.Stat(shmPath); err != nil {
		t.Errorf("backing file was not created: %v", err)
	} else if st.Size() != 16<<20 {
		t.Errorf("backing file is %d bytes, want %d", st.Size(), 16<<20)
	}
}

// The cgroup limit is derived from the same windows that get rendered, so a
// declared window has to show up in the VMM overhead in full.
func TestIvshmemVMMOverhead(t *testing.T) {
	t.Parallel()

	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("NewV4 failed: %v", err)
	}
	adapters := []types.IoAdapter{{Type: types.IoOther, Name: "amba_shm"}}
	aa := types.AssignableAdapters{
		Initialized: true,
		IoBundleList: []types.IoBundle{
			{
				Type:            types.IoOther,
				AssignmentGroup: "amba_shm",
				Phylabel:        "amba_shm",
				Logicallabel:    "amba_shm",
				Cbattr:          map[string]string{"shmpath": "/dev/shm/amba-virt", "shmsize": "256M"},
				UsedByUUID:      id,
			},
		},
	}

	got, err := ivshmemVMMOverhead(DefaultDomainName, &aa, adapters, id)
	if err != nil {
		t.Fatalf("ivshmemVMMOverhead failed: %v", err)
	}
	if want := int64(256 << 20); got != want {
		t.Errorf("ivshmemVMMOverhead = %d, want %d (the whole window, not a fraction)", got, want)
	}

	// A domain with no window pays nothing.
	got, err = ivshmemVMMOverhead(DefaultDomainName, &aa, nil, id)
	if err != nil {
		t.Fatalf("ivshmemVMMOverhead failed: %v", err)
	}
	if got != 0 {
		t.Errorf("ivshmemVMMOverhead = %d for a domain with no windows, want 0", got)
	}
}
