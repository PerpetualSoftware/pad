package store

import "strings"

// IsEmailOptedOut returns true if the given email address has unsubscribed.
func (s *Store) IsEmailOptedOut(email string) (bool, error) {
	var count int
	err := s.db.QueryRow(s.q(`SELECT COUNT(*) FROM email_optouts WHERE email = ?`), strings.ToLower(email)).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

// OptOutEmail adds an email address to the opt-out list.
func (s *Store) OptOutEmail(email string) error {
	_, err := s.db.Exec(s.q(`
		INSERT INTO email_optouts (email) VALUES (?)
		ON CONFLICT (email) DO NOTHING
	`), strings.ToLower(email))
	return err
}
