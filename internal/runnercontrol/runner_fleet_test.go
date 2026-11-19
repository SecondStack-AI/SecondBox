package runnercontrol

import (
	"errors"
	"testing"
	"time"
)

func TestRetiredRunnerCertificateCannotOpenAnotherConnection(t *testing.T) {
	store := openRunnerControlDatabase(t)
	now := time.Now().UTC()
	seedRunnerConnectionForDataPlaneDisconnect(t, store, "fleet-retired-prior", "fleet-retired-prior", now)
	if _, err := store.pool.Exec(t.Context(), `UPDATE secondbox.runners SET fleet_state='retired',retired_at=$2 WHERE id=$1`, "runner-home", now); err != nil {
		t.Fatal(err)
	}
	err := store.OpenConnection(t.Context(), RunnerIdentity{RunnerID: "runner-home", CredentialSerial: "still-valid-certificate"}, "fleet-retired-new", 8, now)
	if !errors.Is(err, ErrRunnerRetired) {
		t.Fatalf("retired identity connection error=%v", err)
	}
	var count int
	if err := store.pool.QueryRow(t.Context(), `SELECT count(*) FROM secondbox.runner_connections WHERE id='fleet-retired-new'`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("retired identity created a control connection")
	}
}
