package config

import "testing"

func TestParseApps(t *testing.T) {
	apps, err := parseApps("shop:token-a, blog:token-b", "")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 2 || apps[0].Name != "shop" || apps[1].Name != "blog" {
		t.Fatalf("apps = %+v", apps)
	}
	if apps[0].Hash != TokenHash("token-a") {
		t.Fatalf("hash = %q", apps[0].Hash)
	}

	// NIGHTWATCH_TOKEN alone becomes the "default" app.
	apps, err = parseApps("", "my-secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Name != "default" || apps[0].Hash != "c27c052" {
		t.Fatalf("apps = %+v", apps)
	}

	// DW_APPS reusing the default token doesn't duplicate it.
	apps, err = parseApps("shop:my-secret-token", "my-secret-token")
	if err != nil {
		t.Fatal(err)
	}
	if len(apps) != 1 || apps[0].Name != "shop" {
		t.Fatalf("apps = %+v", apps)
	}

	// No tokens at all: validation disabled.
	if apps, err = parseApps("", ""); err != nil || len(apps) != 0 {
		t.Fatalf("apps = %+v, err = %v", apps, err)
	}

	for _, bad := range []string{"noseparator", ":token", "name:", "a:t1,a:t2"} {
		if _, err := parseApps(bad, ""); err == nil {
			t.Fatalf("parseApps(%q) should fail", bad)
		}
	}
	// Two apps sharing one token collide on the hash.
	if _, err := parseApps("a:same,b:same", ""); err == nil {
		t.Fatal("shared token should fail")
	}
}
