package app

import "testing"

func TestRegisterRecoveryEmailPersistsUntilCredentialsImported(t *testing.T) {
	root := t.TempDir()
	svc := newRegisterService(root)
	if err := svc.recordRecoveryEmail("partial@icloud.com", "login path=/log-in/password"); err != nil {
		t.Fatal(err)
	}
	reloaded := newRegisterService(root)
	items := reloaded.snapshot().RecoveryEmails
	if len(items) != 1 || items[0].Email != "partial@icloud.com" {
		t.Fatal("recovery email was lost after restart")
	}
	if err := reloaded.clearRecoveryEmail("partial@icloud.com"); err != nil {
		t.Fatal(err)
	}
	if len(newRegisterService(root).snapshot().RecoveryEmails) != 0 {
		t.Fatal("recovery email remained after completion")
	}
}
