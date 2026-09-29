package store

import "testing"

// Suppressing and setting a threshold write different columns of one row; each
// must leave the other alone, and clearing both must leave no row behind.
func TestWarningPrefColumnsAreIndependent(t *testing.T) {
	st := spellStore(t)
	if err := st.SetWarningThreshold("turned-away", "", 10); err != nil {
		t.Fatal(err)
	}
	if err := st.SetWarningSuppress("turned-away", "", 5_000); err != nil {
		t.Fatal(err)
	}
	prefs, err := st.WarningPrefs()
	if err != nil {
		t.Fatal(err)
	}
	if len(prefs) != 1 || prefs[0].Threshold != 10 || prefs[0].SuppressUntilMS != 5_000 {
		t.Fatalf("prefs = %+v, want one row holding both", prefs)
	}

	if err := st.SetWarningSuppress("turned-away", "", 0); err != nil {
		t.Fatal(err)
	}
	if v, _ := st.WarningThreshold("turned-away"); v != 10 {
		t.Errorf("lifting the suppression changed the threshold to %v", v)
	}
	if err := st.SetWarningThreshold("turned-away", "", 0); err != nil {
		t.Fatal(err)
	}
	if prefs, _ := st.WarningPrefs(); len(prefs) != 0 {
		t.Errorf("an empty row survived: %+v", prefs)
	}
}

// A suppression on one model must not hide the same warning on another.
func TestWarningSuppressIsPerSubject(t *testing.T) {
	st := spellStore(t)
	if err := st.SetWarningSuppress("queue-clash", "a", SuppressForever); err != nil {
		t.Fatal(err)
	}
	prefs, _ := st.WarningPrefs()
	if len(prefs) != 1 || prefs[0].Subject != "a" {
		t.Fatalf("prefs = %+v", prefs)
	}
	if !prefs[0].SuppressedAt(1 << 60) {
		t.Error("forever lapsed")
	}
}

func TestWarningSuppressLapses(t *testing.T) {
	p := WarningPref{SuppressUntilMS: 1_000}
	if !p.SuppressedAt(999) || p.SuppressedAt(1_000) {
		t.Error("suppression should hold before its time and lapse at it")
	}
	if (WarningPref{}).SuppressedAt(0) {
		t.Error("a zero row is suppressed")
	}
}

// The multiple is the operator's tolerance: raising it must stop a moderate
// stretch from counting.
func TestSlowSpellMultipleIsTheTolerance(t *testing.T) {
	st := spellStore(t)
	for i := int64(0); i < 100; i++ {
		add(t, st, 1_000+i, 1_000, 200)
	}
	add(t, st, 5_000, 5_000, 200) // ~5x the mean
	if got, _ := st.SlowSpell(Window{}, 0); got.Count != 1 {
		t.Errorf("default 3x: count = %d, want 1", got.Count)
	}
	if got, _ := st.SlowSpell(Window{}, 10); got.Count != 0 {
		t.Errorf("10x: count = %d, want 0", got.Count)
	}
}
