package core

// BundledDefaults will return defaults.Files() when workstream C lands.
// A standalone B build must not reset defaults/ to an empty directory.
func BundledDefaults() map[string]string { return nil }
