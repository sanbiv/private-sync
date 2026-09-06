package vault

import (
	"os"
	"path/filepath"
	"testing"
)

// Exists("") must never resolve to "./vault.json" in the process working
// directory: an unset vault path is not a vault, even when the cwd holds one.
func TestExistsEmptyDir(t *testing.T) {
	_, dir := newVault(t)
	t.Chdir(dir)
	if !Exists(".") {
		t.Fatal("Exists(\".\") = false inside a vault directory")
	}
	if Exists("") {
		t.Error("Exists(\"\") = true; the empty path must not fall back to the working directory")
	}
	if _, err := os.Stat(filepath.Join(dir, VaultFileName)); err != nil {
		t.Fatalf("vault.json missing: %v", err)
	}
}

// A machine info file is named by its owner (a machine only writes its own
// file). A decrypted document claiming another id is corrupt or copied over
// the wrong name: it is skipped with a warning naming the file, exactly like a
// journal claiming another machine is refused. An empty id is filled in from
// the file name.
func TestListMachinesClaimedIDMismatch(t *testing.T) {
	v, dir := newVault(t)
	if err := v.WriteMachine(MachineInfo{ID: mA, Name: "desk"}); err != nil {
		t.Fatal(err)
	}
	impostor := machinePath(mB)
	ct, err := v.SealDoc(impostor, []byte(`{"id":"`+mC+`","name":"impostor"}`))
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, impostor, ct)
	anonymous := machinePath(mC)
	ct, err = v.SealDoc(anonymous, []byte(`{"name":"no id field"}`))
	if err != nil {
		t.Fatal(err)
	}
	writeRaw(t, dir, anonymous, ct)

	ms, warnings, err := v.ListMachines()
	if err != nil {
		t.Fatalf("ListMachines: %v", err)
	}
	if len(ms) != 2 || ms[0].ID != mA || ms[0].Name != "desk" || ms[1].ID != mC || ms[1].Name != "no id field" {
		t.Errorf("ListMachines = %+v, want desk (%s) and the id-less file as %s", ms, mA, mC)
	}
	if !hasWarning(warnings, impostor) || !hasWarning(warnings, mC) {
		t.Errorf("warnings = %v, want one naming %s and the claimed id", warnings, impostor)
	}
	if hasWarning(warnings, anonymous) {
		t.Errorf("id-less machine file produced a warning: %v", warnings)
	}
}
