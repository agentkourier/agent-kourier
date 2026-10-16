package v1alpha1

// AlertmanagerTriggerForTest exposes the package tests' hand-written Alertmanager trigger to the external test package,
// which compares it with chattest.AlertmanagerTrigger: chattest imports this package, so the internal tests cannot.
func AlertmanagerTriggerForTest() Trigger { return alertmanagerTrigger() }
