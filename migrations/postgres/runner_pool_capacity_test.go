package postgresmigrations

import "testing"

func TestRunnerPoolCapacityRemovalPreservesPlacementAndRunnerCapacity(t *testing.T) {
	connection := newGuardDatabase(t)
	lineage, err := readEmbeddedMigrations()
	if err != nil {
		t.Fatal(err)
	}
	for _, item := range lineage[1:] {
		if item.filename == "0025_remove_runner_pool_capacity_policy.sql" {
			break
		}
		applyMigrations(t, connection, item.filename)
	}
	seedRunnerCapacity(t, connection, `{"VCPUCount":8,"MemoryBytes":1073741824}`, `{"VCPUCount":2,"MemoryBytes":268435456}`)
	var poolBefore, runnerBefore string
	if err := connection.QueryRow(t.Context(), `SELECT (to_jsonb(p)-'capacity_policy_json')::text FROM secondbox.runner_pools p WHERE name='pool-vcpu'`).Scan(&poolBefore); err != nil {
		t.Fatal(err)
	}
	if err := connection.QueryRow(t.Context(), `SELECT to_jsonb(r)::text FROM secondbox.runners r WHERE id='runner-vcpu'`).Scan(&runnerBefore); err != nil {
		t.Fatal(err)
	}
	applyMigrations(t, connection, "0025_remove_runner_pool_capacity_policy.sql")
	var poolAfter, runnerAfter string
	if err := connection.QueryRow(t.Context(), `SELECT to_jsonb(p)::text FROM secondbox.runner_pools p WHERE name='pool-vcpu'`).Scan(&poolAfter); err != nil {
		t.Fatal(err)
	}
	if err := connection.QueryRow(t.Context(), `SELECT to_jsonb(r)::text FROM secondbox.runners r WHERE id='runner-vcpu'`).Scan(&runnerAfter); err != nil {
		t.Fatal(err)
	}
	if poolAfter != poolBefore || runnerAfter != runnerBefore {
		t.Fatal("removing pool metadata changed placement or Runner capacity evidence")
	}
	var retained bool
	if err := connection.QueryRow(t.Context(), `SELECT EXISTS (SELECT 1 FROM information_schema.columns WHERE table_schema='secondbox' AND table_name='runner_pools' AND column_name='capacity_policy_json')`).Scan(&retained); err != nil {
		t.Fatal(err)
	}
	if retained {
		t.Fatal("unused pool capacity column remains")
	}
}
