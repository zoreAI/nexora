package app

import "testing"

func TestRegisterPendingPasswordSurvivesRestartUntilAccountImport(t *testing.T) {
	root := t.TempDir()
	svc := newRegisterService(root)
	if err := svc.savePendingPassword("alias@icloud.com", "generated-password", "提交待确认"); err != nil {
		t.Fatal(err)
	}
	reloaded := newRegisterService(root)
	pending := reloaded.snapshot().PendingPasswords
	if len(pending) != 1 || pending[0].Password != "generated-password" || pending[0].State != "提交待确认" {
		t.Fatal("pending password was lost after restart")
	}
	if err := reloaded.savePendingPassword("alias@icloud.com", "generated-password", "注册接口已接受"); err != nil {
		t.Fatal(err)
	}
	pending = newRegisterService(root).snapshot().PendingPasswords
	if len(pending) != 1 || pending[0].State != "注册接口已接受" {
		t.Fatal("accepted state was not persisted")
	}
	if err := reloaded.clearPendingPassword("alias@icloud.com"); err != nil {
		t.Fatal(err)
	}
	if len(newRegisterService(root).snapshot().PendingPasswords) != 0 {
		t.Fatal("pending password remained after account import")
	}
}
