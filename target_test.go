package main

import "testing"

// 回归 P0-1 的根因：不带 -d 时也必须把 -s 的值作为目标。
// 旧实现只在 -d / -sub 分支写 TargetUrl，-s 单独使用时目标为空。
func TestResolveTargetUsesFlagURLWithoutDetectLocation(t *testing.T) {
	restore := saveTargetFlags()
	defer restore()

	*targetInput = "https://example.com/path"
	*detectLocation = false
	*subscribe = ""

	got, err := resolveTarget()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://example.com/path" {
		t.Fatalf("resolveTarget = %q, want the -s value", got)
	}
}

func TestResolveTargetTrimsWhitespace(t *testing.T) {
	restore := saveTargetFlags()
	defer restore()

	*targetInput = "  https://example.com/x \n"
	*detectLocation = false
	*subscribe = ""

	got, err := resolveTarget()
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != "https://example.com/x" {
		t.Fatalf("resolveTarget = %q, want the trimmed url", got)
	}
}

func TestResolveTargetErrorsWithoutTarget(t *testing.T) {
	restore := saveTargetFlags()
	defer restore()

	*targetInput = ""
	*detectLocation = false
	*subscribe = ""

	if _, err := resolveTarget(); err == nil {
		t.Fatal("expected an error when neither -s nor -sub is given")
	}
}

func TestValidateTarget(t *testing.T) {
	bad := []string{
		"",
		"example.com",
		"ftp://example.com",
		"http://",
		"://nope",
	}
	for _, in := range bad {
		if err := validateTarget(in); err == nil {
			t.Errorf("validateTarget(%q) should fail", in)
		}
	}

	good := []string{
		"http://example.com",
		"https://example.com:8443/path?q=1",
		"http://127.0.0.1:18080/x",
	}
	for _, in := range good {
		if err := validateTarget(in); err != nil {
			t.Errorf("validateTarget(%q) = %v, want nil", in, err)
		}
	}
}

func TestValidateFlagsRejectsPortInCustomIP(t *testing.T) {
	restore := saveTargetFlags()
	defer restore()

	oldIP := customIP
	defer func() { customIP = oldIP }()

	customIP = ipArray{"10.0.0.1:8080"}
	if err := validateFlags(); err == nil {
		t.Fatal("expected an error for -i containing a port")
	}

	customIP = ipArray{"10.0.0.1", "::1"}
	if err := validateFlags(); err != nil {
		t.Fatalf("unexpected error for valid addresses: %v", err)
	}
}

func saveTargetFlags() func() {
	oldTarget, oldDetect, oldSub := *targetInput, *detectLocation, *subscribe
	return func() {
		*targetInput, *detectLocation, *subscribe = oldTarget, oldDetect, oldSub
	}
}
