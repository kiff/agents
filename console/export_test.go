package console

// Seal is seal, for tests that make a cookie the console didn't.
func (s *SignIn) Seal(purpose string, v any) (string, error) { return s.seal(purpose, v) }
