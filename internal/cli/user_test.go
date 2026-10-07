package cli_test

import (
	"bytes"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/ad3002/flylab/internal/auth"
	"github.com/ad3002/flylab/internal/cli"
	"github.com/ad3002/flylab/internal/storage"
)

func run(t *testing.T, store *storage.Store, args ...string) (int, map[string]interface{}, string) {
	t.Helper()
	var out, errOut bytes.Buffer
	code := cli.RunUser(args, store, &out, &errOut)
	var parsed map[string]interface{}
	line := out.String()
	if code != 0 {
		line = errOut.String()
	}
	if strings.Count(strings.TrimSpace(line), "\n") != 0 {
		t.Fatalf("output must be one line, got %q", line)
	}
	if err := json.Unmarshal([]byte(line), &parsed); err != nil {
		t.Fatalf("output is not JSON: %q (%v)", line, err)
	}
	return code, parsed, line
}

func TestUserCreateAndPasswd(t *testing.T) {
	store, err := storage.OpenStore(filepath.Join(t.TempDir(), "cli.db"))
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	defer store.Close()

	code, res, line := run(t, store, "create", "--username", "Demo", "--password", "secret-pass", "--display-name", "Demo User")
	if code != 0 || res["ok"] != true || res["action"] != "created" {
		t.Fatalf("create failed: %s", line)
	}
	user := res["user"].(map[string]interface{})
	if user["username"] != "demo" || user["display_name"] != "Demo User" {
		t.Fatalf("unexpected user %v", user)
	}
	if strings.Contains(line, "password") || strings.Contains(line, "pbkdf2") {
		t.Fatalf("output leaks password material: %s", line)
	}

	code, res, _ = run(t, store, "create", "--username", "demo", "--password", "another-pass")
	if code == 0 || !strings.Contains(res["error"].(string), "already taken") {
		t.Fatalf("duplicate create must fail with 'already taken', got code=%d %v", code, res)
	}

	if err := store.CreateSession(int64(user["id"].(float64)), auth.HashToken("tok"), timeNowPlusHour()); err != nil {
		t.Fatalf("create session: %v", err)
	}
	code, res, line = run(t, store, "passwd", "--username", "demo", "--password", "new-password-1")
	if code != 0 || res["action"] != "password_changed" || res["sessions_revoked"] != float64(1) {
		t.Fatalf("passwd failed: %s", line)
	}
	_, hash, err := store.GetUserCredentials("demo")
	if err != nil {
		t.Fatalf("credentials: %v", err)
	}
	if ok, err := auth.VerifyPassword("new-password-1", hash); err != nil || !ok {
		t.Fatalf("new password does not verify: ok=%v err=%v", ok, err)
	}

	code, res, _ = run(t, store, "passwd", "--username", "ghost", "--password", "whatever-1")
	if code == 0 || !strings.Contains(res["error"].(string), "does not exist") {
		t.Fatalf("passwd for unknown user must fail, got %v", res)
	}
	code, res, _ = run(t, store, "create", "--username", "x", "--password", "short")
	if code == 0 || !strings.Contains(res["error"].(string), "username must be") {
		t.Fatalf("invalid username must fail, got %v", res)
	}
	code, res, _ = run(t, store, "delete", "--username", "demo")
	if code == 0 || !strings.Contains(res["error"].(string), "unknown subcommand") {
		t.Fatalf("unknown subcommand must fail, got %v", res)
	}
}

func timeNowPlusHour() time.Time { return time.Now().Add(time.Hour) }
