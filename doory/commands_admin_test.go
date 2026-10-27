package doory

import (
	"bytes"
	"strings"
	"testing"
)

func TestMakeAdminNormalizesEmail(t *testing.T) {
	env := newAdminEnv(t)
	user := env.makeUser("LiYu@Example.com", false, false) // createUser stores lower-case
	var stdout, stderr bytes.Buffer
	if code := RunCLI(env.app, []string{"make-admin", "  LiYu@Example.COM  "}, &stdout, &stderr); code != 0 {
		t.Fatalf("make-admin exit = %d\nstdout: %s\nstderr: %s", code, stdout.String(), stderr.String())
	}
	reloaded, err := getUserByID(env.db, user.ID)
	if err != nil || reloaded == nil {
		t.Fatalf("reload: %v", err)
	}
	if !reloaded.IsStaff || reloaded.IsSuperuser {
		t.Fatalf("flags = staff:%v superuser:%v, want staff only", reloaded.IsStaff, reloaded.IsSuperuser)
	}
	if !strings.Contains(stdout.String(), "liyu@example.com is now an admin.") {
		t.Errorf("stdout = %q", stdout.String())
	}
}
