package config

import "testing"

func TestDeriveJWKSURL(t *testing.T) {
	cases := map[string]string{
		"https://host:8443/tid/v2.0":                 "https://host:8443/tid/discovery/v2.0/keys",
		"https://host:8443/tid/v2.0/":                "https://host:8443/tid/discovery/v2.0/keys",
		"https://login.microsoftonline.com/tid/v2.0": "https://login.microsoftonline.com/tid/discovery/v2.0/keys",
		"https://host/other":                         "https://host/other/discovery/v2.0/keys",
	}
	for issuer, want := range cases {
		if got := DeriveJWKSURL(issuer); got != want {
			t.Errorf("DeriveJWKSURL(%q) = %q; want %q", issuer, got, want)
		}
	}
}

func TestFinishRequiresIssuer(t *testing.T) {
	c := &Config{}
	if err := c.Finish(); err == nil {
		t.Fatal("Finish() without issuer succeeded; want error")
	}
	c.EntraIssuer = "https://h/t/v2.0"
	if err := c.Finish(); err != nil {
		t.Fatal(err)
	}
	if c.EntraJWKSURL != "https://h/t/discovery/v2.0/keys" {
		t.Fatalf("JWKS not derived: %q", c.EntraJWKSURL)
	}
	if c.RetryAfterSeconds != 1 {
		t.Fatalf("RetryAfterSeconds default = %d; want 1", c.RetryAfterSeconds)
	}
}

func TestFromEnv(t *testing.T) {
	t.Setenv("FABRIC_ENTRA_ISSUER", "https://e:1/t/v2.0")
	t.Setenv("FABRIC_ENTRA_TLS_INSECURE", "true")
	t.Setenv("FABRIC_ADDR", ":9999")
	t.Setenv("FABRIC_AIRFLOW_URL", "http://airflow:8080")
	t.Setenv("FABRIC_AIRFLOW_DAG_DIR", "/dags")
	t.Setenv("FABRIC_AIRFLOW_USERNAME", "fabric")
	t.Setenv("FABRIC_AIRFLOW_PASSWORD", "secret")
	t.Setenv("FABRIC_MLFLOW_URL", "http://mlflow:5000")
	t.Setenv("FABRIC_KQL_URL", "http://kustainer:8080")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.Addr != ":9999" || !c.EntraTLSInsecure || c.EntraIssuer != "https://e:1/t/v2.0" {
		t.Fatalf("FromEnv misread env: %+v", c)
	}
	if c.AirflowURL != "http://airflow:8080" || c.AirflowDAGDir != "/dags" || c.AirflowUsername != "fabric" || c.AirflowPassword != "secret" {
		t.Fatalf("FromEnv misread Airflow env: %+v", c)
	}
	if c.MLflowURL != "http://mlflow:5000" {
		t.Fatalf("FromEnv misread MLflow env: %+v", c)
	}
	if c.KQLURL != "http://kustainer:8080" {
		t.Fatalf("FromEnv misread the Kusto engine env: %+v", c)
	}
}

// TestKQLURLDefaultsOff: no FABRIC_KQL_URL means no engine, which the server
// turns into an honest 501 rather than a silent stub.
func TestKQLURLDefaultsOff(t *testing.T) {
	t.Setenv("FABRIC_ENTRA_ISSUER", "https://e:1/t/v2.0")
	t.Setenv("FABRIC_KQL_URL", "")
	c, err := FromEnv()
	if err != nil {
		t.Fatal(err)
	}
	if c.KQLURL != "" {
		t.Fatalf("KQLURL = %q, want empty", c.KQLURL)
	}
}

func TestFromEnvMissingIssuer(t *testing.T) {
	t.Setenv("FABRIC_ENTRA_ISSUER", "")
	if _, err := FromEnv(); err == nil {
		t.Fatal("FromEnv without issuer succeeded; want error")
	}
}

func TestBoolEnvShapes(t *testing.T) {
	for v, want := range map[string]bool{"1": true, "true": true, "YES": true, "on": true, "0": false, "": false, "no": false} {
		t.Setenv("FABRIC_TEST_BOOL", v)
		if got := boolEnv("FABRIC_TEST_BOOL"); got != want {
			t.Errorf("boolEnv(%q) = %v; want %v", v, got, want)
		}
	}
}

func TestTSQLStrictFromEnv(t *testing.T) {
	t.Setenv("FABRIC_TSQL_STRICT", "true")
	if !FromEnvPartial().TSQLStrict {
		t.Fatal("FABRIC_TSQL_STRICT=true did not enable strict mode")
	}
	t.Setenv("FABRIC_TSQL_STRICT", "")
	if FromEnvPartial().TSQLStrict {
		t.Fatal("strict mode is on by default; it must be opt-in")
	}
}

func TestListPageSizeFromEnv(t *testing.T) {
	t.Setenv("FABRIC_LIST_PAGE_SIZE", "2")
	if got := FromEnvPartial().ListPageSize; got != 2 {
		t.Fatalf("got %d, want 2", got)
	}
	t.Setenv("FABRIC_LIST_PAGE_SIZE", "")
	if got := FromEnvPartial().ListPageSize; got != 0 {
		t.Fatalf("unset should mean 0 (use the default), got %d", got)
	}
	t.Setenv("FABRIC_LIST_PAGE_SIZE", "not-a-number")
	if got := FromEnvPartial().ListPageSize; got != 0 {
		t.Fatalf("garbage should fall back to the default, got %d", got)
	}
	t.Setenv("FABRIC_LIST_PAGE_SIZE", "-1")
	if got := FromEnvPartial().ListPageSize; got != -1 {
		t.Fatalf("negative should pass through to disable paging, got %d", got)
	}
}

// TestBuildNamesOneIdentityWhicheverPathBuiltIt.
//
// The point of the string is that a screenshot, a recording and a bug report
// all quote the SAME thing. That only holds if the two build paths converge:
// GoReleaser stamps `0.15.2`, while the image build passes `${{ github.ref_name }}`,
// which is `v0.15.2`. If those rendered differently, two reports of one build
// would read as reports of two.
func TestBuildNamesOneIdentityWhicheverPathBuiltIt(t *testing.T) {
	sha := "eef154a9c3d1b0000000000000000000000000ab"
	for _, tc := range []struct{ name, version, commit, want string }{
		{"goreleaser", "0.15.2", sha, "v0.15.2-eef154a"},
		{"image build", "v0.15.2", sha, "v0.15.2-eef154a"},
		{"source build", "dev", "", "dev"},
		{"unstamped", "", "", "dev"},
		{"tag with no commit", "0.15.2", "", "v0.15.2"},
		{"short commit already", "0.15.2", "abc123", "v0.15.2-abc123"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &Config{Version: tc.version, Commit: tc.commit}
			if got := c.Build(); got != tc.want {
				t.Fatalf("Build() = %q, want %q", got, tc.want)
			}
		})
	}
}
