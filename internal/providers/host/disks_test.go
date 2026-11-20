// SPDX-FileCopyrightText: 2026 Mauktik Gandhi
// SPDX-License-Identifier: Apache-2.0

package host

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"slices"
	"testing"

	"github.com/mauktik/nexus/internal/problem"
	"github.com/mauktik/nexus/internal/providers/host/spec"
)

// diskList is host.disks.list in one page of 1000, checked by decodeList.
func (s *hostStack) diskList() []spec.Disk {
	s.t.Helper()
	out, err := s.p.disks(context.Background(), spec.ListInput{Limit: 1000})
	if err != nil {
		s.t.Fatalf("disks handler: %v", err)
	}
	l := decodeList[spec.DiskProperties](s.t, spec.DisksType, out, nil)
	if l.NextCursor != "" {
		s.t.Fatalf("limit 1000: nextCursor %q", l.NextCursor)
	}
	return l.Items
}

func diskNames(items []spec.Disk) []string {
	out := make([]string, len(items))
	for i, d := range items {
		out[i] = d.Name
	}
	return out
}

func diskAt(t *testing.T, items []spec.Disk, name string) spec.DiskProperties {
	t.Helper()
	for _, d := range items {
		if d.Name == name {
			return d.Properties
		}
	}
	t.Fatalf("disk %s is not listed: %v", name, diskNames(items))
	return spec.DiskProperties{}
}

func partNames(ps []spec.Partition) []string {
	var out []string
	for _, p := range ps {
		out = append(out, p.Name)
	}
	return out
}

// lsblkDev is one device of `lsblk -b -J -o NAME,SIZE,ROTA,RM,MODEL,TYPE,MOUNTPOINTS`.
type lsblkDev struct {
	Name        string     `json:"name"`
	Size        uint64     `json:"size"`
	Rota        bool       `json:"rota"`
	RM          bool       `json:"rm"`
	Model       *string    `json:"model"`
	Type        string     `json:"type"`
	Mountpoints []*string  `json:"mountpoints"`
	Children    []lsblkDev `json:"children"`
}

// mountPoints is the device's mount points, sorted, without lsblk's null
// placeholder.
func (d lsblkDev) mountPoints() []string {
	var out []string
	for _, m := range d.Mountpoints {
		if m != nil {
			out = append(out, *m)
		}
	}
	slices.Sort(out)
	return out
}

// lsblkDisks is the capture's tools/lsblk devices of type disk.
func lsblkDisks(t *testing.T, capture string) []lsblkDev {
	t.Helper()
	var doc struct {
		Blockdevices []lsblkDev `json:"blockdevices"`
	}
	if err := json.Unmarshal(capFile(t, capture, "tools", "lsblk"), &doc); err != nil {
		t.Fatalf("tools/lsblk: %v", err)
	}
	var out []lsblkDev
	for _, d := range doc.Blockdevices {
		if d.Type == "disk" {
			out = append(out, d)
		}
	}
	return out
}

func TestParseDev(t *testing.T) {
	for in, want := range map[string]devOf{"253:0\n": {253, 0}, "8:16": {8, 16}, " 259:3 \n": {259, 3}} {
		if got, err := parseDev([]byte(in)); err != nil || got != want {
			t.Errorf("parseDev(%q) = %v, %v", in, got, err)
		}
	}
	for _, in := range []string{"", "8", "8:", ":1", "a:b", "8:1:2", "4294967296:0"} {
		if _, err := parseDev([]byte(in)); err == nil {
			t.Errorf("parseDev(%q) accepted", in)
		}
	}
}

// An unreadable sys/block fails the list as a dependency.
func TestDisksSysBlockUnreadable(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("sys/block")
	_, err := s.p.disks(context.Background(), spec.ListInput{})
	var p *problem.Error
	if !errors.As(err, &p) || p.Slug() != problem.DependencyUnavailable {
		t.Errorf("err = %v, want dependency-unavailable", err)
	}
}

// ram and zram devices are never listed; a loop is listed only with a
// backing file.
func TestDisksExclusions(t *testing.T) {
	s := newHostStack(t, capUBU)
	for _, d := range []string{"ram0", "zram0"} {
		s.ffs.replace("sys/block/"+d+"/size", []byte("8\n"))
		s.ffs.replace("sys/block/"+d+"/dev", []byte("1:0\n"))
	}
	s.ffs.replace("sys/block/loop3/loop/backing_file", []byte("/srv/image\n"))
	if got, want := diskNames(s.diskList()), []string{"loop3", "vda", "vdb", "vdc"}; !slices.Equal(got, want) {
		t.Errorf("disks %v, want %v", got, want)
	}
}

// Partitions are ordered by partition number, not by name.
func TestDiskPartitionsByNumber(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.replace("sys/block/vda/vda2/partition", []byte("2\n"))
	s.ffs.replace("sys/block/vda/vda2/size", []byte("2048\n"))
	s.ffs.replace("sys/block/vda/vda2/dev", []byte("253:2\n"))
	vda := diskAt(t, s.diskList(), "vda")
	if got, want := partNames(vda.Partitions), []string{"vda1", "vda2", "vda15", "vda16"}; !slices.Equal(got, want) {
		t.Errorf("partitions %v, want %v", got, want)
	}
	if p := vda.Partitions[1]; p.SizeBytes == nil || *p.SizeBytes != 2048*512 || len(p.MountPoints) != 0 {
		t.Errorf("vda2 = %+v", p)
	}
}

// Rates join on name and maj:min: a device now at another maj:min under
// the same name is warming up, never given the old device's rates.
func TestDiskRatesJoinOnMajMin(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ready()
	s.ffs.replace("sys/block/vda/dev", []byte("253:99\n"))
	items := s.diskList()
	vda := diskAt(t, items, "vda")
	if vda.ReadBytesPerSecond != nil || vda.Unavailable["readBytesPerSecond"] != "warming up" {
		t.Errorf("vda read %s with %q, want warming up", show(vda.ReadBytesPerSecond), vda.Unavailable["readBytesPerSecond"])
	}
	if vdb := diskAt(t, items, "vdb"); vdb.ReadBytesPerSecond == nil {
		t.Errorf("vdb read null with %q", vdb.Unavailable["readBytesPerSecond"])
	}
}

// Without the sampler the rates are not running and the rest is read live.
func TestDisksSamplerNotRunning(t *testing.T) {
	s := newHostStack(t, capUBU)
	for _, d := range s.diskList() {
		p := d.Properties
		if p.Sampler.State != spec.SamplerNotRunning || p.Unavailable["writeIops"] != "not running" || p.SizeBytes == nil {
			t.Errorf("%s: sampler %q, writeIops %q, size %s", d.Name, p.Sampler.State, p.Unavailable["writeIops"],
				show(p.SizeBytes))
		}
	}
}

// Without proc/1/mountinfo the partition mount points come from nexusd's
// own mountinfo; without either they are [] with the reason.
func TestDiskPartitionMountsFallback(t *testing.T) {
	s := newHostStack(t, capUBU)
	s.ffs.deny("proc/1/mountinfo")
	vdb := diskAt(t, s.diskList(), "vdb")
	if len(vdb.Partitions) != 1 || len(vdb.Partitions[0].MountPoints) != 5 {
		t.Errorf("vdb partitions %+v, want vdb1 with its 5 mount points", vdb.Partitions)
	}
	s.ffs.deny("proc/self/mountinfo")
	vdb = diskAt(t, s.diskList(), "vdb")
	if p := vdb.Partitions[0]; len(p.MountPoints) != 0 || p.Unavailable["mountPoints"] != "permission denied: /proc/1/mountinfo" {
		t.Errorf("vdb1 = %+v", p)
	}
}

// The disks list pages by name with nginx's cursor.
func TestDisksPaging(t *testing.T) {
	s := newHostStack(t, capUBU)
	var got []string
	cursor := ""
	for {
		out, err := s.p.disks(context.Background(), spec.ListInput{Limit: 1, Cursor: cursor})
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, diskNames(out.Items)...)
		if out.NextCursor == "" {
			break
		}
		if want := base64.RawURLEncoding.EncodeToString([]byte(out.Items[0].Name)); out.NextCursor != want {
			t.Errorf("cursor %q, want %q", out.NextCursor, want)
		}
		cursor = out.NextCursor
	}
	if want := []string{"vda", "vdb", "vdc"}; !slices.Equal(got, want) {
		t.Errorf("pages %v, want %v", got, want)
	}
}
