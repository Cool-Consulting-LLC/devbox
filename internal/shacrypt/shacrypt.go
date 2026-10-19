// Package shacrypt makes SHA-512 crypt(5) hashes ($6$), what `openssl passwd -6`
// and glibc's crypt(3) make, so the sudo password hash needs no OpenSSL 3 on the
// laptop. It follows Drepper's specification with the default 5000 rounds.
package shacrypt

import (
	"crypto/rand"
	"crypto/sha512"
	"strings"
)

const itoa64 = "./0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"

// Hash returns the $6$ hash of password with a random 16-character salt.
func Hash(password []byte) string {
	salt := make([]byte, 16)
	rand.Read(salt) // never fails
	for i, r := range salt {
		salt[i] = itoa64[r&0x3f]
	}
	return hash(password, salt)
}

// repeat is the spec's P and S sequences: digest repeated to n bytes.
func repeat(digest []byte, n int) []byte {
	out := make([]byte, 0, n)
	for ; n >= len(digest); n -= len(digest) {
		out = append(out, digest...)
	}
	return append(out, digest[:n]...)
}

func hash(key, salt []byte) string {
	b := sha512.New()
	b.Write(key)
	b.Write(salt)
	b.Write(key)
	db := b.Sum(nil)

	a := sha512.New()
	a.Write(key)
	a.Write(salt)
	a.Write(repeat(db, len(key)))
	for n := len(key); n > 0; n >>= 1 {
		if n&1 != 0 {
			a.Write(db)
		} else {
			a.Write(key)
		}
	}
	sum := a.Sum(nil)

	dp := sha512.New()
	for range key {
		dp.Write(key)
	}
	p := repeat(dp.Sum(nil), len(key))

	ds := sha512.New()
	for range 16 + int(sum[0]) {
		ds.Write(salt)
	}
	s := repeat(ds.Sum(nil), len(salt))

	for i := range 5000 {
		c := sha512.New()
		if i&1 != 0 {
			c.Write(p)
		} else {
			c.Write(sum)
		}
		if i%3 != 0 {
			c.Write(s)
		}
		if i%7 != 0 {
			c.Write(p)
		}
		if i&1 != 0 {
			c.Write(sum)
		} else {
			c.Write(p)
		}
		sum = c.Sum(nil)
	}

	var out strings.Builder
	out.WriteString("$6$")
	out.Write(salt)
	out.WriteByte('$')
	b64 := func(b2, b1, b0 byte, n int) {
		w := uint(b2)<<16 | uint(b1)<<8 | uint(b0)
		for range n {
			out.WriteByte(itoa64[w&0x3f])
			w >>= 6
		}
	}
	for i := range 21 {
		// The spec's byte order: 0,21,42 then 22,43,1 then 44,2,23, and so on.
		x, y, z := i, (i+21)%63, (i+42)%63
		switch i % 3 {
		case 1:
			x, y, z = (i+21)%63, (i+42)%63, i
		case 2:
			x, y, z = (i+42)%63, i, (i+21)%63
		}
		b64(sum[x], sum[y], sum[z], 4)
	}
	b64(0, 0, sum[63], 2)
	return out.String()
}
