package postgresmigrations

import (
	"testing"
	"time"
)

func TestPersistedAuthoritySchemaUsesOnlyRequiredIdentityConstraints(t *testing.T) {
	connection, databaseURL := newDisposableDatabase(t)
	if err := Apply(t.Context(), databaseURL); err != nil {
		t.Fatal(err)
	}
	rows, err := connection.Query(t.Context(), `
		SELECT class.relname,con.contype::text
		FROM pg_catalog.pg_constraint AS con
		JOIN pg_catalog.pg_class AS class ON class.oid=con.conrelid
		JOIN pg_catalog.pg_namespace AS namespace ON namespace.oid=class.relnamespace
		WHERE namespace.nspname='secondbox'
		  AND class.relname = ANY($1)
		ORDER BY class.relname,con.conname`, []string{
		"tenants", "subjects", "authority_identities",
		"tenant_controller_authorities", "application_authorities",
	})
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	constraints := map[string][]string{}
	for rows.Next() {
		var table, kind string
		if err := rows.Scan(&table, &kind); err != nil {
			t.Fatal(err)
		}
		constraints[table] = append(constraints[table], kind)
		if kind == "f" || kind == "c" || kind == "x" {
			t.Errorf("secondbox.%s has forbidden %s constraint", table, kind)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{
		"tenants", "subjects", "authority_identities",
		"tenant_controller_authorities", "application_authorities",
	} {
		if len(constraints[table]) == 0 {
			t.Errorf("secondbox.%s has no identity constraint", table)
		}
	}

	now := time.Date(2026, 8, 25, 12, 0, 0, 0, time.UTC)
	for _, tenantRef := range []string{"tenant-a", "tenant-b"} {
		if _, err := connection.Exec(t.Context(), `
			INSERT INTO secondbox.tenants (
				ref,state,allowed_profile_grants_json,allowed_application_scopes_json,
				aggregate_quota_json,expiry_policy_json,metadata_json,revision,created_at,updated_at
			) VALUES ($1,'active','[]','[]','{}','{}','{}',1,$2,$2)`, tenantRef, now,
		); err != nil {
			t.Fatal(err)
		}
		if _, err := connection.Exec(t.Context(), `
			INSERT INTO secondbox.subjects (
				tenant_ref,ref,state,cleanup_state,quota_json,metadata_json,revision,created_at,updated_at
			) VALUES ($1,'same-subject','active','none','{}','{}',1,$2,$2)`, tenantRef, now,
		); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.subjects (
			tenant_ref,ref,state,cleanup_state,quota_json,metadata_json,revision,created_at,updated_at
		) VALUES ('tenant-a','same-subject','active','none','{}','{}',1,$1,$1)`, now,
	); err == nil {
		t.Fatal("duplicate tenant-local subject identity was accepted")
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.subjects (
			tenant_ref,ref,state,cleanup_state,quota_json,metadata_json,revision,created_at,updated_at
		) VALUES ('logical-missing-tenant','logical-subject','active','none','{}','{}',1,$1,$1)`, now,
	); err != nil {
		t.Fatalf("logical cross-resource reference was physically constrained: %v", err)
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.authority_identities (id,kind,lookup_id)
		VALUES ('authority-global','application','apa_global_lookup')`,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.authority_identities (id,kind,lookup_id)
		VALUES ('authority-global','tenant_controller','tca_other_lookup')`,
	); err == nil {
		t.Fatal("duplicate cross-kind authority identity was accepted")
	}
	if _, err := connection.Exec(t.Context(), `
		INSERT INTO secondbox.authority_identities (id,kind,lookup_id)
		VALUES ('authority-other','tenant_controller','apa_global_lookup')`,
	); err == nil {
		t.Fatal("duplicate cross-kind lookup identity was accepted")
	}
}
