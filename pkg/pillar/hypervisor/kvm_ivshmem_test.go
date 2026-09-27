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

	// PFN-backed windows are character devices (e.g. /dev/amba_virt_shm).
	// They must be openable without error even if st_size is 0.
	if err := ensureSharedMemoryFile(ivshmemWindow{
		id: "pfn", memPath: "/dev/null", size: 16 << 20,
	}); err != nil {
		t.Errorf("character-device backing failed: %v", err)
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

	aa.IoBundleList[0].Cbattr["shmpath"] = "/dev/null"
	got, err = ivshmemVMMOverhead(DefaultDomainName, &aa, adapters, id)
	if err != nil {
		t.Fatalf("ivshmemVMMOverhead for PFN window failed: %v", err)
	}
	if got != 0 {
		t.Errorf("ivshmemVMMOverhead = %d for PFN-backed window, want 0", got)
	}
}

func TestUartAdapterFromBundle(t *testing.T) {
	t.Parallel()

	// Valid UART2 bundle (Ubuntu HVM target).
	u, err := uartAdapterFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "UART2",
		Logicallabel: "UART2",
		Cbattr: map[string]string{
			"uart": "2",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u == nil {
		t.Fatal("expected a uartAdapter for UART2 bundle")
	}
	if u.id != "UART2" || u.uartID != "2" || u.hostDevice != "ffe0018000.uart" {
		t.Errorf("got %+v, want id=UART2 uartID=2 hostDevice=ffe0018000.uart", *u)
	}

	// Valid UART3 bundle (QNX HVM target).
	u, err = uartAdapterFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "UART3",
		Logicallabel: "UART3",
		Cbattr: map[string]string{
			"uart": "3",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u == nil || u.uartID != "3" || u.hostDevice != "ffe0019000.uart" {
		t.Errorf("got %+v, want uartID=3 hostDevice=ffe0019000.uart", *u)
	}

	// UART0 is host management console (filtered from guest HVM).
	u, err = uartAdapterFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "UART0",
		Logicallabel: "UART0",
		Cbattr: map[string]string{
			"uart": "0",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != nil {
		t.Errorf("expected UART0 to be filtered from guest assignment, got %+v", *u)
	}

	// Mutual exclusion: UART bundles must NOT be treated as bulk shared-memory windows.
	w, err := ivshmemWindowFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "UART2",
		Logicallabel: "UART2",
		Cbattr: map[string]string{
			"uart": "2",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if w != nil {
		t.Errorf("UART bundle was wrongly treated as a bulk shared-memory window: %+v", *w)
	}

	// Bulk shared-memory window must NOT be treated as a UART adapter.
	u, err = uartAdapterFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "amba_shm",
		Logicallabel: "amba_shm",
		Cbattr: map[string]string{
			"shmpath": "/dev/amba_virt_shm",
			"shmsize": "1G",
		},
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if u != nil {
		t.Errorf("bulk shm bundle was wrongly treated as a UART adapter: %+v", *u)
	}

	// Conflicting attributes: combining uart with shmpath/shmsize must fail.
	_, err = ivshmemWindowFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "bad_uart",
		Logicallabel: "bad_uart",
		Cbattr: map[string]string{
			"uart":    "2",
			"shmpath": "/dev/shm/foo",
		},
	})
	if err == nil {
		t.Error("expected error when combining uart and shmpath in ivshmemWindowFromBundle")
	}

	_, err = uartAdapterFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "bad_uart",
		Logicallabel: "bad_uart",
		Cbattr: map[string]string{
			"uart":    "2",
			"shmpath": "/dev/shm/foo",
		},
	})
	if err == nil {
		t.Error("expected error when combining uart and shmpath in uartAdapterFromBundle")
	}

	// Unsupported UART ID (e.g. 5).
	_, err = uartAdapterFromBundle(types.IoBundle{
		Type:         types.IoOther,
		Phylabel:     "bad_uart",
		Logicallabel: "bad_uart",
		Cbattr: map[string]string{
			"uart": "5",
		},
	})
	if err == nil {
		t.Error("expected error for unsupported uart ID 5")
	}
}

func TestCreateDomConfigWithUart(t *testing.T) {
	t.Parallel()

	conf, err := os.CreateTemp("/tmp", "config")
	if err != nil {
		t.Fatalf("can't create config file for a domain %v", err)
	}
	defer os.Remove(conf.Name())

	diskConfigs, diskStatuses := qemuDisks()
	config, aa := domainConfigAndAssignableAdapters(diskConfigs)
	config.VirtualizationMode = types.HVM

	config.IoAdapterList = append(config.IoAdapterList, types.IoAdapter{
		Type: types.IoOther,
		Name: "UART2",
	})
	aa.IoBundleList = append(aa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "uart2",
		Phylabel:        "UART2",
		Logicallabel:    "UART2",
		Cbattr:          map[string]string{"uart": "2"},
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

	// Must contain direct vfio-platform UART device and DMA32 lease window
	for _, want := range []string{
		`[device "vfio-uart2"]`,
		`driver = "vfio-platform"`,
		`host = "ffe0018000.uart"`,
		`[object "dma32-lease0"]`,
		`qom-type = "memory-backend-file"`,
		`mem-path = "/dev/amba_dma_lease0"`,
		`size = "16777216"`,
		`share = "on"`,
		`[device "dma32-lease0-dev"]`,
		`driver = "ivshmem-plain"`,
		`memdev = "dma32-lease0"`,
	} {
		if !strings.Contains(got, want) {
			t.Errorf("generated config is missing %q:\n%s", want, got)
		}
	}

	// Must NOT contain legacy proxy ivshmem-doorbell or socket
	for _, unwanted := range []string{
		`ivshmem-doorbell`,
		`/run/amba_virt_uart2.sock`,
	} {
		if strings.Contains(got, unwanted) {
			t.Errorf("generated config contains legacy proxy directive %q:\n%s", unwanted, got)
		}
	}
}

func TestUartPassthroughVMMOverhead(t *testing.T) {
	t.Parallel()

	id, err := uuid.NewV4()
	if err != nil {
		t.Fatalf("NewV4 failed: %v", err)
	}
	adapters := []types.IoAdapter{{Type: types.IoOther, Name: "UART2"}}
	aa := types.AssignableAdapters{
		Initialized: true,
		IoBundleList: []types.IoBundle{
			{
				Type:            types.IoOther,
				AssignmentGroup: "uart2",
				Phylabel:        "UART2",
				Logicallabel:    "UART2",
				Cbattr: map[string]string{
					"uart": "2",
				},
				UsedByUUID: id,
			},
		},
	}

	got, err := uartPassthroughVMMOverhead(DefaultDomainName, &aa, adapters, id)
	if err != nil {
		t.Fatalf("uartPassthroughVMMOverhead failed: %v", err)
	}
	if want := int64(16 << 20); got != want {
		t.Errorf("uartPassthroughVMMOverhead = %d, want %d (16 MiB lease slice)", got, want)
	}

	// Domain with no UART adapters has 0 UART overhead
	aaNoUart := types.AssignableAdapters{Initialized: true}
	got, err = uartPassthroughVMMOverhead(DefaultDomainName, &aaNoUart, nil, id)
	if err != nil {
		t.Fatalf("uartPassthroughVMMOverhead failed: %v", err)
	}
	if got != 0 {
		t.Errorf("uartPassthroughVMMOverhead for empty adapters = %d, want 0", got)
	}
}

func TestUartLeaseReused(t *testing.T) {
	testDomain := DefaultDomainName
	defer releaseDmaLease(testDomain)

	diskConfigs, diskStatuses := qemuDisks()
	config, aa := domainConfigAndAssignableAdapters(diskConfigs)
	config.VirtualizationMode = types.HVM
	config.IoAdapterList = append(config.IoAdapterList, types.IoAdapter{
		Type: types.IoOther,
		Name: "UART2",
	})
	aa.IoBundleList = append(aa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "uart2",
		Phylabel:        "UART2",
		Logicallabel:    "UART2",
		Cbattr:          map[string]string{"uart": "2"},
		UsedByUUID:      config.UUIDandVersion.UUID,
	})

	conf1, err := os.CreateTemp("/tmp", "config1")
	if err != nil {
		t.Fatalf("can't create config1 file: %v", err)
	}
	defer os.Remove(conf1.Name())

	if err := kvmArm.CreateDomConfig(testDomain, config, types.DomainStatus{},
		diskStatuses, &aa, nil, swtpmCtrlSock, conf1); err != nil {
		t.Fatalf("first CreateDomConfig failed: %v", err)
	}

	result1, err := os.ReadFile(conf1.Name())
	if err != nil {
		t.Fatalf("reading conf1 file failed: %v", err)
	}
	got1 := string(result1)

	// Extract CID from first config
	cid1 := ""
	for _, line := range strings.Split(got1, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "guest-cid =") {
			cid1 = strings.TrimSpace(line)
			break
		}
	}
	if cid1 == "" {
		t.Fatalf("first config missing guest-cid:\n%s", got1)
	}
	if !strings.Contains(got1, `/dev/amba_dma_lease0`) {
		t.Fatalf("first config missing /dev/amba_dma_lease0:\n%s", got1)
	}

	// Second CreateDomConfig for the SAME domain name
	conf2, err := os.CreateTemp("/tmp", "config2")
	if err != nil {
		t.Fatalf("can't create config2 file: %v", err)
	}
	defer os.Remove(conf2.Name())

	if err := kvmArm.CreateDomConfig(testDomain, config, types.DomainStatus{},
		diskStatuses, &aa, nil, swtpmCtrlSock, conf2); err != nil {
		t.Fatalf("second CreateDomConfig failed: %v", err)
	}

	result2, err := os.ReadFile(conf2.Name())
	if err != nil {
		t.Fatalf("reading conf2 file failed: %v", err)
	}
	got2 := string(result2)

	cid2 := ""
	for _, line := range strings.Split(got2, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "guest-cid =") {
			cid2 = strings.TrimSpace(line)
			break
		}
	}
	if cid2 == "" {
		t.Fatalf("second config missing guest-cid:\n%s", got2)
	}
	if cid1 != cid2 {
		t.Errorf("second config CID changed: got %s, want %s", cid2, cid1)
	}
	if !strings.Contains(got2, `/dev/amba_dma_lease0`) {
		t.Fatalf("second config missing /dev/amba_dma_lease0:\n%s", got2)
	}

	// A different domain name gets its own distinct CID and lease
	otherDomain := "11111111-2222-3333-4444-555555555555.0.0"
	defer releaseDmaLease(otherDomain)

	conf3, err := os.CreateTemp("/tmp", "config3")
	if err != nil {
		t.Fatalf("can't create config3 file: %v", err)
	}
	defer os.Remove(conf3.Name())

	otherConfig, otherAa := domainConfigAndAssignableAdapters(diskConfigs)
	otherID, err := uuid.FromString("11111111-2222-3333-4444-555555555555")
	if err != nil {
		t.Fatalf("failed to parse UUID: %v", err)
	}
	otherConfig.UUIDandVersion.UUID = otherID
	otherConfig.VirtualizationMode = types.HVM
	for i := range otherAa.IoBundleList {
		otherAa.IoBundleList[i].UsedByUUID = otherID
	}
	otherConfig.IoAdapterList = append(otherConfig.IoAdapterList, types.IoAdapter{
		Type: types.IoOther,
		Name: "UART2",
	})
	otherAa.IoBundleList = append(otherAa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "uart2",
		Phylabel:        "UART2",
		Logicallabel:    "UART2",
		Cbattr:          map[string]string{"uart": "2"},
		UsedByUUID:      otherID,
	})

	if err := kvmArm.CreateDomConfig(otherDomain, otherConfig, types.DomainStatus{},
		diskStatuses, &otherAa, nil, swtpmCtrlSock, conf3); err != nil {
		t.Fatalf("third CreateDomConfig for other domain failed: %v", err)
	}

	result3, err := os.ReadFile(conf3.Name())
	if err != nil {
		t.Fatalf("reading conf3 file failed: %v", err)
	}
	got3 := string(result3)

	cid3 := ""
	for _, line := range strings.Split(got3, "\n") {
		if strings.HasPrefix(strings.TrimSpace(line), "guest-cid =") {
			cid3 = strings.TrimSpace(line)
			break
		}
	}
	if cid3 == cid1 {
		t.Errorf("other domain unexpectedly reused cid %s", cid3)
	}
}

func TestDomConfigFailurePreservesFile(t *testing.T) {
	testDomain := DefaultDomainName
	defer releaseDmaLease(testDomain)

	conf, err := os.CreateTemp("/tmp", "config-atomic")
	if err != nil {
		t.Fatalf("can't create temp config file: %v", err)
	}
	defer os.Remove(conf.Name())

	diskConfigs, diskStatuses := qemuDisks()
	config, aa := domainConfigAndAssignableAdapters(diskConfigs)
	config.VirtualizationMode = types.HVM
	config.IoAdapterList = append(config.IoAdapterList, types.IoAdapter{
		Type: types.IoOther,
		Name: "UART2",
	})
	aa.IoBundleList = append(aa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "uart2",
		Phylabel:        "UART2",
		Logicallabel:    "UART2",
		Cbattr:          map[string]string{"uart": "2"},
		UsedByUUID:      config.UUIDandVersion.UUID,
	})

	// Initial successful generation
	if err := kvmArm.CreateDomConfig(testDomain, config, types.DomainStatus{},
		diskStatuses, &aa, nil, swtpmCtrlSock, conf); err != nil {
		t.Fatalf("initial CreateDomConfig failed: %v", err)
	}

	origBytes, err := os.ReadFile(conf.Name())
	if err != nil {
		t.Fatalf("failed to read original config: %v", err)
	}
	if len(origBytes) == 0 {
		t.Fatal("original config is empty")
	}

	// Injected failure: pass invalid adapter configuration that triggers failure during generation
	badAa := aa
	badAa.IoBundleList = append(badAa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "bad",
		Phylabel:        "bad",
		Logicallabel:    "bad",
		Cbattr:          map[string]string{"uart": "invalid-id"},
		UsedByUUID:      config.UUIDandVersion.UUID,
	})
	badConfig := config
	badConfig.IoAdapterList = append(badConfig.IoAdapterList, types.IoAdapter{
		Type: types.IoOther,
		Name: "bad",
	})

	err = kvmArm.CreateDomConfig(testDomain, badConfig, types.DomainStatus{},
		diskStatuses, &badAa, nil, swtpmCtrlSock, conf)
	if err == nil {
		t.Fatal("expected CreateDomConfig to fail for invalid adapter bundle")
	}

	// Verify that the file content on disk is completely preserved and unchanged
	newBytes, err := os.ReadFile(conf.Name())
	if err != nil {
		t.Fatalf("failed to read config after failure: %v", err)
	}
	if string(newBytes) != string(origBytes) {
		t.Fatalf("config file was corrupted or modified on failure:\nGot:\n%s\nWant:\n%s",
			string(newBytes), string(origBytes))
	}
}

func TestDomConfigRejectsRegularFileAtLeasePath(t *testing.T) {
	testDomain := DefaultDomainName
	defer releaseDmaLease(testDomain)

	// Create a dummy control device file so kvm.go knows to enforce char device check
	dummyCtl, err := os.CreateTemp("/tmp", "mock_amba_dma_ctl")
	if err != nil {
		t.Fatalf("failed to create dummy ctl file: %v", err)
	}
	defer os.Remove(dummyCtl.Name())

	// Create a dummy regular file at the mock lease path
	dummyLease, err := os.CreateTemp("/tmp", "mock_amba_dma_lease")
	if err != nil {
		t.Fatalf("failed to create dummy lease file: %v", err)
	}
	defer os.Remove(dummyLease.Name())

	// Override paths for testing
	origCtl := ambaDmaCtlDev
	origLeaseFunc := ambaDmaLeasePathFunc
	ambaDmaCtlDev = dummyCtl.Name()
	ambaDmaLeasePathFunc = func(leaseID uint32) string {
		return dummyLease.Name()
	}
	defer func() {
		ambaDmaCtlDev = origCtl
		ambaDmaLeasePathFunc = origLeaseFunc
	}()

	conf, err := os.CreateTemp("/tmp", "config-reject")
	if err != nil {
		t.Fatalf("can't create temp config file: %v", err)
	}
	defer os.Remove(conf.Name())

	_ = saveDomainLease(testDomain, &domainDmaLeaseInfo{
		LeaseID: 0,
		Epoch:   1,
		Cid:     10,
	})

	diskConfigs, diskStatuses := qemuDisks()
	config, aa := domainConfigAndAssignableAdapters(diskConfigs)
	config.VirtualizationMode = types.HVM
	config.IoAdapterList = append(config.IoAdapterList, types.IoAdapter{
		Type: types.IoOther,
		Name: "UART2",
	})
	aa.IoBundleList = append(aa.IoBundleList, types.IoBundle{
		Type:            types.IoOther,
		AssignmentGroup: "uart2",
		Phylabel:        "UART2",
		Logicallabel:    "UART2",
		Cbattr:          map[string]string{"uart": "2"},
		UsedByUUID:      config.UUIDandVersion.UUID,
	})

	err = kvmArm.CreateDomConfig(testDomain, config, types.DomainStatus{},
		diskStatuses, &aa, nil, swtpmCtrlSock, conf)
	if err == nil {
		t.Fatal("expected CreateDomConfig to fail when lease path is a regular file, but it succeeded")
	}

	errMsg := err.Error()
	if !strings.Contains(errMsg, dummyLease.Name()) || !strings.Contains(errMsg, "regular file") {
		t.Fatalf("expected error message to name the path and 'regular file', got: %s", errMsg)
	}

	// Verify that the stanza was NOT written to the config file
	confBytes, _ := os.ReadFile(conf.Name())
	if strings.Contains(string(confBytes), "dma32-lease") {
		t.Fatalf("dma32-lease stanza was unexpectedly written to config file on error: %s", string(confBytes))
	}
}
