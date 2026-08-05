package postgresmigrations

import "testing"

func TestProxiedPortTransportMigrationRewritesRowsAndDefault(t *testing.T) {
	connection := newGuardDatabase(t)
	applyMigrations(t, connection,
		"0002_sandbox_name_index.sql",
		"0003_control_plane_wakeups.sql",
		"0004_direct_port_data_plane.sql",
	)
	insertWakeupPortSession(t, connection, "relay")
	applyMigrations(t, connection, "0012_proxied_port_transport.sql")

	var transport string
	if err := connection.QueryRow(t.Context(), `
		SELECT transport FROM secondbox.port_sessions WHERE id='relay'`,
	).Scan(&transport); err != nil {
		t.Fatal(err)
	}
	if transport != "proxied" {
		t.Fatalf("migrated Port transport = %q, want proxied", transport)
	}

	var defaultExpression string
	if err := connection.QueryRow(t.Context(), `
		SELECT column_default
		FROM information_schema.columns
		WHERE table_schema='secondbox' AND table_name='port_sessions' AND column_name='transport'`,
	).Scan(&defaultExpression); err != nil {
		t.Fatal(err)
	}
	if defaultExpression != "'proxied'::text" {
		t.Fatalf("Port transport default = %q, want proxied", defaultExpression)
	}
}
