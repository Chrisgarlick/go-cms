package webhooks

import "crypto/rand"

func cryptoRand(b []byte) (int, error) { return rand.Read(b) }
