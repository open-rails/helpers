package smtp

// NewImplicitTLS is New as if Port were 465: a test server cannot bind it.
func NewImplicitTLS(cfg Config) (*Sender, error) {
	s, err := New(cfg)
	if err == nil {
		s.implicit = true
	}
	return s, err
}
