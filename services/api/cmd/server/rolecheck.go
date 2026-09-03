package main

import (
	"context"
	"fmt"
	"log/slog"

	"github.com/jackc/pgx/v5/pgxpool"
)

// assertRolePosture refuses to start the server unless Postgres itself confirms
// that row-level security is being ENFORCED on the request pool and BYPASSED on
// the system pool.
//
// Why this exists rather than a config read: "RLS is enabled" and "RLS applies
// to us" are different claims, and only the second one matters. infra/init.sql
// runs 30 `ENABLE ROW LEVEL SECURITY` statements, and for the entire life of
// this project they were decoration — the API connected as `auditor`, which is
// initdb's bootstrap SUPERUSER *and* the owner of every table, and Postgres
// exempts both unconditionally. Nothing in the code, the schema, or the test
// suite could see that, because every policy was simply never consulted.
//
// So this asks the server what it enforces, on the actual pooled connections the
// application will use, and it does so with a NEGATIVE CONTROL: the same
// row_security_active() probe must come back true on the app pool and false on
// the sys pool. A probe that returns the same answer on both is not measuring
// anything, and that outcome is treated as a failure too.
func assertRolePosture(ctx context.Context, appPool, sysPool *pgxpool.Pool) error {
	app, err := inspectRole(ctx, appPool)
	if err != nil {
		return fmt.Errorf("inspecting DATABASE_URL role: %w", err)
	}
	sys, err := inspectRole(ctx, sysPool)
	if err != nil {
		return fmt.Errorf("inspecting SYS_DATABASE_URL role: %w", err)
	}

	slog.Info("database role posture",
		"app_role", app.name, "app_superuser", app.superuser,
		"app_bypassrls", app.bypassRLS, "app_rls_active", app.rlsActive,
		"app_owns_table", app.ownsProbeTable, "app_force_rls", app.forceRLS,
		"sys_role", sys.name, "sys_superuser", sys.superuser,
		"sys_bypassrls", sys.bypassRLS, "sys_rls_active", sys.rlsActive,
		"policies_in_public_schema", app.policyCount)

	// --- request pool: policies must apply ---------------------------------
	if app.superuser {
		return fmt.Errorf("DATABASE_URL connects as %q which is a SUPERUSER; "+
			"superusers bypass row security unconditionally, so all %d policies "+
			"would be inert. Point DATABASE_URL at auditor_app (see .env.example)",
			app.name, app.policyCount)
	}
	if app.bypassRLS {
		return fmt.Errorf("DATABASE_URL connects as %q which has BYPASSRLS; "+
			"all %d policies would be inert. That attribute belongs only on "+
			"auditor_sys (SYS_DATABASE_URL)", app.name, app.policyCount)
	}
	if !app.rlsActive {
		return fmt.Errorf("row_security_active(%q) is FALSE for role %q — the "+
			"request pool is not subject to row security. Most likely cause: the "+
			"role owns the table and the table is not FORCE ROW LEVEL SECURITY "+
			"(owns_table=%t, force_rls=%t)",
			rlsProbeTable, app.name, app.ownsProbeTable, app.forceRLS)
	}
	if !app.forceRLS {
		// Reachable when the app role is a non-owner: policies apply today, but a
		// later `ALTER TABLE ... OWNER TO auditor_app` would silently turn them
		// off again. init.sql's $force_rls$ loop sets this on every RLS table.
		return fmt.Errorf("%s does not have FORCE ROW LEVEL SECURITY; "+
			"infra/init.sql is expected to set it on every RLS table", rlsProbeTable)
	}
	if app.policyCount == 0 {
		return fmt.Errorf("row security is active for %q but the public schema "+
			"contains ZERO policies — every SELECT would return zero rows. The "+
			"schema in infra/init.sql was probably not applied", app.name)
	}

	// --- system pool: must be able to cross tenants, but not be a superuser --
	if sys.superuser {
		return fmt.Errorf("SYS_DATABASE_URL connects as %q which is a SUPERUSER; "+
			"it only needs BYPASSRLS. Point it at auditor_sys", sys.name)
	}
	if !sys.bypassRLS {
		return fmt.Errorf("SYS_DATABASE_URL connects as %q which lacks BYPASSRLS; "+
			"login, signup, password reset, the client portal and all three "+
			"background workers run on this pool and have no single firm to scope "+
			"to, so every policy predicate would raise on the unset "+
			"app.current_firm GUC", sys.name)
	}

	// --- negative control ---------------------------------------------------
	// If the probe cannot tell the two pools apart it is not a probe.
	if sys.rlsActive == app.rlsActive {
		return fmt.Errorf("row_security_active(%q) returned %t on BOTH pools — "+
			"the two DSNs are resolving to the same role posture (app=%q, sys=%q), "+
			"so this check proves nothing about tenant isolation",
			rlsProbeTable, app.rlsActive, app.name, sys.name)
	}
	if app.name == sys.name {
		return fmt.Errorf("DATABASE_URL and SYS_DATABASE_URL both connect as %q; "+
			"they must be auditor_app and auditor_sys respectively", app.name)
	}

	slog.Info("RLS posture verified — policies are enforced on the request pool",
		"probe", fmt.Sprintf("row_security_active('%s')", rlsProbeTable),
		"app", app.name, "app_result", app.rlsActive,
		"sys", sys.name, "sys_result", sys.rlsActive)
	return nil
}

// rlsProbeTable is the table the posture check interrogates. client_books is the
// right choice: it is the root of the two-level tenancy model, it carries a
// firm_id policy, and any deployment that has run init.sql has it.
const rlsProbeTable = "client_books"

type rolePosture struct {
	name           string
	superuser      bool
	bypassRLS      bool
	rlsActive      bool
	ownsProbeTable bool
	forceRLS       bool
	policyCount    int
}

// inspectRole asks the server what it enforces for one pool.
//
// row_security_active() is deliberately used instead of "run a SELECT and see
// whether it errors". The obvious probe — SELECT count(*) FROM client_books with
// no GUC set, expecting current_setting() to raise — is WRONG: Postgres
// evaluates a USING clause per row, so on an empty table (every fresh install,
// and CI) the query succeeds with 0 and the probe concludes RLS is inert. That
// would refuse to boot exactly the deployments that are correctly configured.
// row_security_active is data-independent and already accounts for superuser,
// BYPASSRLS, and owner-without-FORCE.
func inspectRole(ctx context.Context, pool *pgxpool.Pool) (rolePosture, error) {
	var p rolePosture
	err := pool.QueryRow(ctx, `
		SELECT current_user,
		       r.rolsuper,
		       r.rolbypassrls,
		       row_security_active($1::regclass),
		       c.relowner = r.oid,
		       c.relforcerowsecurity,
		       (SELECT count(*) FROM pg_policies WHERE schemaname = 'public')
		FROM pg_roles r, pg_class c
		WHERE r.rolname = current_user
		  AND c.oid = $1::regclass`, rlsProbeTable).
		Scan(&p.name, &p.superuser, &p.bypassRLS, &p.rlsActive,
			&p.ownsProbeTable, &p.forceRLS, &p.policyCount)
	if err != nil {
		// A missing probe table is the single most likely failure here and the
		// generic pgx message ("relation ... does not exist") does not say what to
		// do about it, so name the cause.
		return p, fmt.Errorf("posture query failed (is infra/init.sql applied? "+
			"table %q must exist): %w", rlsProbeTable, err)
	}
	return p, nil
}
