package web

import "testing"

func TestParseTrace(t *testing.T) {
	data := map[string]any{
		"trace": `[
			{"file":"routes/web.php:33","source":"","code":{"32":"Route::get('/boom', function () {","33":"    throw new RuntimeException('x');","34":"});"}},
			{"file":"vendor/laravel/framework/src/Illuminate/Routing/Route.php:254","source":"Illuminate\\Routing\\CallableDispatcher->dispatch()","code":null}
		]`,
	}
	frames := parseTrace(data)
	if len(frames) != 2 {
		t.Fatalf("got %d frames, want 2", len(frames))
	}

	f := frames[0]
	if f.File != "routes/web.php" || f.Line != 33 {
		t.Fatalf("frame 0 location = %s:%d", f.File, f.Line)
	}
	if !f.App {
		t.Fatal("routes/web.php should be an app frame")
	}
	if len(f.Code) != 3 || f.Code[0].No != 32 || f.Code[2].No != 34 {
		t.Fatalf("code lines wrong: %+v", f.Code)
	}
	if !f.Code[1].Current || f.Code[0].Current {
		t.Fatal("current-line marking wrong")
	}

	v := frames[1]
	if v.App {
		t.Fatal("vendor frame marked as app")
	}
	if v.Line != 254 || v.Source == "" || v.Code != nil {
		t.Fatalf("vendor frame parsed wrong: %+v", v)
	}
}

func TestParseTraceMalformed(t *testing.T) {
	for _, data := range []map[string]any{
		{},
		{"trace": ""},
		{"trace": "not json"},
		{"trace": 42.0},
	} {
		if got := parseTrace(data); got != nil {
			t.Fatalf("parseTrace(%v) = %v, want nil", data, got)
		}
	}
}
