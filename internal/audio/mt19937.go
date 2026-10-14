package audio

// MT19937 with the 2002 array-seeding recurrence. Fixed-width operations wrap
// modulo 2^32, which keeps the sequence independent of the host word size.
// Algorithm reference: Matsumoto and Nishimura,
// https://www.math.sci.hiroshima-u.ac.jp/m-mat/MT/MT2002/emt19937ar.html
type mt19937 struct {
	state [624]uint32
	index int
}

func newMT19937(key []uint32) mt19937 {
	var mt mt19937
	mt.state[0] = 19650218
	for i := 1; i < len(mt.state); i++ {
		previous := mt.state[i-1]
		mt.state[i] = 1812433253*(previous^(previous>>30)) + uint32(i)
	}
	i, j := 1, 0
	advance := func() {
		i++
		if i == len(mt.state) {
			mt.state[0] = mt.state[len(mt.state)-1]
			i = 1
		}
	}
	for remaining := max(len(mt.state), len(key)); remaining > 0; remaining-- {
		previous := mt.state[i-1]
		mt.state[i] = (mt.state[i] ^ (1664525 * (previous ^ (previous >> 30)))) + key[j] + uint32(j)
		advance()
		j = (j + 1) % len(key)
	}
	for remaining := len(mt.state) - 1; remaining > 0; remaining-- {
		previous := mt.state[i-1]
		mt.state[i] = (mt.state[i] ^ (1566083941 * (previous ^ (previous >> 30)))) - uint32(i)
		advance()
	}
	mt.state[0] = 0x80000000
	mt.index = len(mt.state)
	return mt
}

func (mt *mt19937) next() uint32 {
	if mt.index == len(mt.state) {
		for i := range mt.state {
			joined := (mt.state[i] & 0x80000000) | (mt.state[(i+1)%len(mt.state)] & 0x7fffffff)
			value := mt.state[(i+397)%len(mt.state)] ^ (joined >> 1)
			if joined&1 != 0 {
				value ^= 0x9908b0df
			}
			mt.state[i] = value
		}
		mt.index = 0
	}
	value := mt.state[mt.index]
	mt.index++
	value ^= value >> 11
	value ^= (value << 7) & 0x9d2c5680
	value ^= (value << 15) & 0xefc60000
	return value ^ (value >> 18)
}
