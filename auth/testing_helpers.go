package auth

// SetAccountsForTest 直接替换内存账号列表，仅供其它包的测试使用。
func (s *Store) SetAccountsForTest(accounts []*Account) {
	s.mu.Lock()
	s.accounts = append([]*Account(nil), accounts...)
	s.mu.Unlock()
}
