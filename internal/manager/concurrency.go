package manager

type gate struct{ sem chan struct{} }

func newGate(max int) *gate {
	if max <= 0 {
		max = 1
	}
	return &gate{sem: make(chan struct{}, max)}
}
