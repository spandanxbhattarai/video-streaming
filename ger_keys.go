package main

import (
    "crypto/rand"
    "encoding/hex"
    "fmt"
)

func generateRandomString(n int) string {
    b := make([]byte, n)
    _, err := rand.Read(b)
    if err != nil {
        panic(err)
    }
    return hex.EncodeToString(b)
}

func main() {
    apiKey := generateRandomString(8)    // 16 hex chars
    apiSecret := generateRandomString(16) // 32 hex chars

    fmt.Println("API Key:", apiKey)
    fmt.Println("API Secret:", apiSecret)
}
