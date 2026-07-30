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
