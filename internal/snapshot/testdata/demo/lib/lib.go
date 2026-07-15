package lib

const Limit = 10

type Store struct{ n int }

func (s *Store) Put(v int) { s.n = v }

func Double(v int) int { return v * 2 }

func Tail(v int) int { return v + Limit }
