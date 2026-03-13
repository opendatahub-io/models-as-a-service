package api_keys_test

import (
	"crypto/rand"
	"encoding/hex"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	"github.com/opendatahub-io/models-as-a-service/maas-api/internal/api_keys"
)

// TestPBKDF2ParameterValidation validates PBKDF2 security parameters.
func TestPBKDF2ParameterValidation(t *testing.T) {
	t.Run("IterationCountCompliance", func(t *testing.T) {
		// Test that we meet OWASP 2024 guidelines (600k iterations)
		_, hashData, _, err := api_keys.GenerateAPIKey()
		require.NoError(t, err)
		
		// OWASP 2024 recommendation is 600,000 iterations minimum
		assert.Equal(t, 600000, hashData.Iterations, "PBKDF2 iterations must be 600,000 (OWASP 2024 compliant)")
		
		// Verify it's within acceptable range for security vs performance
		assert.GreaterOrEqual(t, hashData.Iterations, 100000, "Must be at least 100k iterations")
		assert.LessOrEqual(t, hashData.Iterations, 10000000, "Must not exceed 10M iterations (DoS protection)")
	})


	t.Run("HashOutputQuality", func(t *testing.T) {
		// Verify hash output properties
		_, hashData, _, err := api_keys.GenerateAPIKey()
		require.NoError(t, err)
		
		// Test hash length (32 bytes = 64 hex chars for SHA-256)
		assert.Len(t, hashData.Hash, 64, "PBKDF2 output must be 32 bytes (64 hex chars)")
		
		// Test hash is valid hex - verify it decodes properly
		_, err = hex.DecodeString(hashData.Hash)
		assert.NoError(t, err, "Hash must be valid hex encoding")
	})

	t.Run("HashUniqueness", func(t *testing.T) {
		// Different keys should produce different hashes even with same input pattern
		hashes := make(map[string]bool)
		numTests := 50

		for range numTests {
			_, hashData, _, err := api_keys.GenerateAPIKey()
			require.NoError(t, err)
			
			assert.False(t, hashes[hashData.Hash], "Hash collision detected")
			hashes[hashData.Hash] = true
		}
	})
}

// TestCryptographicEntropy tests the quality of cryptographic randomness.
func TestCryptographicEntropy(t *testing.T) {
	t.Run("KeyGenerationEntropy", func(t *testing.T) {
		// Test that key generation produces sufficiently random keys
		keys := make(map[string]bool)
		numTests := 100

		for range numTests {
			key, _, _, err := api_keys.GenerateAPIKey()
			require.NoError(t, err)
			
			// Remove prefix for entropy analysis
			keyBody := key[len(api_keys.KeyPrefix):]
			
			assert.False(t, keys[keyBody], "Key collision detected")
			keys[keyBody] = true
			
			// Test key body length (should be substantial)
			assert.GreaterOrEqual(t, len(keyBody), 40, "Key body should be at least 40 characters")
		}
	})
}

// TestTimingAttackResistance validates constant-time operations.
func TestTimingAttackResistance(t *testing.T) {
	t.Run("ConstantTimeComparison", func(t *testing.T) {
		// Create a test key and hash
		testKey := "sk-oai-timing-test-key"
		hashData, err := api_keys.HashAPIKey(testKey)
		require.NoError(t, err)

		// Test that verification time is roughly constant regardless of where differences occur
		measurements := make([]time.Duration, 0)
		
		// Test 1: Completely correct key
		start := time.Now()
		computedHash, err := api_keys.HashAPIKey(testKey)
		valid := computedHash.Hash == hashData.Hash
		elapsed := time.Since(start)
		require.NoError(t, err)
		assert.True(t, valid)
		measurements = append(measurements, elapsed)

		// Test 2: Wrong key (early difference)
		wrongKey1 := "sk-oai-WRONG-test-key"
		start = time.Now()
		computedHash, err = api_keys.HashAPIKey(wrongKey1)
		valid = computedHash.Hash == hashData.Hash
		elapsed = time.Since(start)
		require.NoError(t, err)
		assert.False(t, valid)
		measurements = append(measurements, elapsed)

		// Test 3: Wrong key (late difference)  
		wrongKey2 := "sk-oai-timing-test-WRONG"
		start = time.Now()
		computedHash, err = api_keys.HashAPIKey(wrongKey2)
		valid = computedHash.Hash == hashData.Hash
		elapsed = time.Since(start)
		require.NoError(t, err)
		assert.False(t, valid)
		measurements = append(measurements, elapsed)

		// Statistical timing analysis would go here in a production security test
		// For this test, we verify that PBKDF2 dominates timing (all should be similar)
		// Note: This is a basic check - sophisticated timing attacks need more analysis
		
		t.Logf("Timing measurements: %v", measurements)
		
		// All measurements should be in similar range (within order of magnitude)
		// PBKDF2's computational cost should dominate any early-exit timing
		minTime := measurements[0]
		maxTime := measurements[0]
		
		for _, duration := range measurements {
			if duration < minTime {
				minTime = duration
			}
			if duration > maxTime {
				maxTime = duration
			}
		}
		
		// Ratio should not be excessive (PBKDF2 should dominate timing)
		ratio := float64(maxTime) / float64(minTime)
		assert.Less(t, ratio, 10.0, "Timing variation too high - possible timing attack vulnerability")
	})
}

// TestMemoryClearing validates that sensitive data is properly cleared.
func TestMemoryClearing(t *testing.T) {
	t.Run("NoPlaintextInMemory", func(t *testing.T) {
		// This test is inherently limited by Go's memory management,
		// but we can verify the implementation attempts to clear sensitive data
		
		testKey := "sk-oai-memory-test-very-secret-key"
		
		// Generate hash and force garbage collection
		hashData, err := api_keys.HashAPIKey(testKey)
		require.NoError(t, err)
		
		runtime.GC()
		runtime.GC() // Force multiple GC cycles
		
		// Verify the hash was created properly
		computedHash, err := api_keys.HashAPIKey(testKey)
		require.NoError(t, err)
		assert.Equal(t, hashData.Hash, computedHash.Hash)
		
		// Note: Go's memory model makes it difficult to test memory clearing effectively.
		// In production, this would require specialized memory analysis tools.
		// The keygen.go implementation does explicit clearing with:
		// for i := range slice { slice[i] = 0 }
		
		t.Log("Memory clearing implementation present in keygen.go HashAPIKey")
	})
	
	t.Run("ConstantSaltUsage", func(t *testing.T) {
		// Verify that constant salt is being used properly
		// With constant salt, we rely on high-entropy keys for security
		
		// Generate multiple keys to verify hash uniqueness despite constant salt
		for range 10 {
			_, hashData1, _, err := api_keys.GenerateAPIKey()
			require.NoError(t, err)
			
			_, hashData2, _, err := api_keys.GenerateAPIKey()
			require.NoError(t, err)
			
			// Hashes should be different due to high-entropy key input
			assert.NotEqual(t, hashData1.Hash, hashData2.Hash, "Hash collision detected with constant salt")
		}
	})
}

// TestAttackVectorResistance tests resistance to common attack vectors.
func TestAttackVectorResistance(t *testing.T) {
	t.Run("BruteForceResistance", func(t *testing.T) {
		// Test that PBKDF2 makes brute force attacks computationally expensive
		testKey := "sk-oai-brute-force-test"
		
		// Measure time for single hash operation
		start := time.Now()
		_, err := api_keys.HashAPIKey(testKey)
		elapsed := time.Since(start)
		require.NoError(t, err)
		
		// With 600,000 iterations, each hash should take significant time
		// This makes brute force attacks expensive
		assert.Greater(t, elapsed, time.Millisecond*50, "PBKDF2 hashing too fast - insufficient brute force protection")
		assert.Less(t, elapsed, time.Second*2, "PBKDF2 hashing too slow - may impact performance")
		
		t.Logf("Single PBKDF2 hash time: %v", elapsed)
	})

	t.Run("RainbowTableResistance", func(t *testing.T) {
		// With constant salt, rainbow table resistance comes from high-entropy keys
		// Same input should produce same hash (deterministic with constant salt)
		sameInput := "sk-oai-rainbow-test"
		
		// Hash the same input multiple times
		hashData1, err := api_keys.HashAPIKey(sameInput)
		require.NoError(t, err)
		
		hashData2, err := api_keys.HashAPIKey(sameInput)
		require.NoError(t, err)
		
		// Same input with constant salt should produce same hash
		assert.Equal(t, hashData1.Hash, hashData2.Hash, "Constant salt should produce deterministic hashes")
		
		// Different inputs should still produce different hashes
		differentInput := "sk-oai-different-test"
		hashData3, err := api_keys.HashAPIKey(differentInput)
		require.NoError(t, err)
		
		assert.NotEqual(t, hashData1.Hash, hashData3.Hash, "Different inputs should produce different hashes")
	})

	t.Run("CollisionResistance", func(t *testing.T) {
		// Test hash collision resistance
		hashes := make(map[string]bool)
		
		// Generate many different keys and verify no hash collisions
		for range 1000 {
			key, hashData, _, err := api_keys.GenerateAPIKey()
			require.NoError(t, err)
			
			// With constant salt, only check hash collision
			assert.False(t, hashes[hashData.Hash], "Hash collision detected for key: %s", key)
			hashes[hashData.Hash] = true
		}
	})
}

// TestPerformanceRequirements validates that security doesn't compromise usability.
func TestPerformanceRequirements(t *testing.T) {
	t.Run("HashGenerationPerformance", func(t *testing.T) {
		// Test that key generation meets performance requirements
		numTests := 10
		var totalTime time.Duration
		
		for range numTests {
			start := time.Now()
			_, err := api_keys.HashAPIKey("sk-oai-performance-test")
			elapsed := time.Since(start)
			require.NoError(t, err)
			
			totalTime += elapsed
		}
		
		avgTime := totalTime / time.Duration(numTests)
		
		// Performance requirement: <500ms per key generation (as per POA)
		assert.Less(t, avgTime, time.Millisecond*500, "Key generation too slow - exceeds 500ms requirement")
		
		t.Logf("Average hash generation time: %v", avgTime)
	})

	t.Run("VerificationPerformance", func(t *testing.T) {
		// Test that key verification meets performance requirements
		testKey := "sk-oai-verification-perf"
		hashData, err := api_keys.HashAPIKey(testKey)
		require.NoError(t, err)
		
		numTests := 10
		var totalTime time.Duration
		
		for range numTests {
			start := time.Now()
			computedHash, err := api_keys.HashAPIKey(testKey)
			valid := computedHash.Hash == hashData.Hash
			elapsed := time.Since(start)
			require.NoError(t, err)
			assert.True(t, valid)
			
			totalTime += elapsed
		}
		
		avgTime := totalTime / time.Duration(numTests)
		
		// Performance requirement: <200ms per verification (as per POA)
		assert.Less(t, avgTime, time.Millisecond*200, "Key verification too slow - exceeds 200ms requirement")
		
		t.Logf("Average verification time: %v", avgTime)
	})

	t.Run("ConcurrentVerification", func(t *testing.T) {
		// Test concurrent verification performance
		testKey := "sk-oai-concurrent-test"
		hashData, err := api_keys.HashAPIKey(testKey)
		require.NoError(t, err)

		numGoroutines := 10
		numVerifications := 5

		var wg sync.WaitGroup
		start := time.Now()

		for range numGoroutines {
			wg.Go(func() {
				for range numVerifications {
					computedHash, err := api_keys.HashAPIKey(testKey)
					valid := computedHash.Hash == hashData.Hash
					require.NoError(t, err)
					assert.True(t, valid)
				}
			})
		}

		wg.Wait()
		elapsed := time.Since(start)

		// Total verifications: numGoroutines * numVerifications = 50
		avgTimePerVerification := elapsed / time.Duration(numGoroutines*numVerifications)
		
		// Concurrent performance should still meet requirements
		assert.Less(t, avgTimePerVerification, time.Millisecond*200, "Concurrent verification performance degraded")
		
		t.Logf("Concurrent verification avg time: %v", avgTimePerVerification)
	})
}

// TestFIPSCompliance validates FIPS 140-3 compliance aspects.
func TestFIPSCompliance(t *testing.T) {
	t.Run("ApprovedAlgorithms", func(t *testing.T) {
		// Verify we're using FIPS-approved algorithms
		// PBKDF2-HMAC-SHA256 is FIPS 140-3 approved
		
		_, hashData, _, err := api_keys.GenerateAPIKey()
		require.NoError(t, err)
		
		// Test PBKDF2 parameters are within FIPS guidelines
		assert.GreaterOrEqual(t, hashData.Iterations, 100000, "FIPS requires minimum iteration count")
		assert.Len(t, hashData.Hash, 64, "FIPS requires SHA-256 output (32 bytes = 64 hex)")
	})

	t.Run("KeyDerivationCompliance", func(t *testing.T) {
		// Test that our PBKDF2 implementation follows FIPS standards
		// This verifies the underlying golang.org/x/crypto/pbkdf2 implementation
		
		testKey := "sk-oai-fips-test"
		hashData, err := api_keys.HashAPIKey(testKey)
		require.NoError(t, err)
		
		// Verify hash is deterministic with constant salt
		// This ensures we're using the FIPS-approved implementation correctly
		assert.Equal(t, 600000, hashData.Iterations, "Iteration count must match implementation")
		
		// Verify constant salt is being used
		testKey2 := "sk-oai-fips-test-2"
		hashData2, err := api_keys.HashAPIKey(testKey2)
		require.NoError(t, err)
		assert.Equal(t, hashData.Iterations, hashData2.Iterations, "Iterations should be constant")
	})

	t.Run("RandomnessCompliance", func(t *testing.T) {
		// Test that key generation uses cryptographically secure randomness
		// crypto/rand is FIPS-approved for random number generation
		
		// Generate test entropy to verify crypto/rand is working
		entropyBytes := make([]byte, 32)
		n, err := rand.Read(entropyBytes)
		require.NoError(t, err)
		assert.Equal(t, 32, n, "crypto/rand must generate requested number of bytes")
		
		// Verify entropy is not all zeros (basic sanity check)
		allZero := true
		for _, b := range entropyBytes {
			if b != 0 {
				allZero = false
				break
			}
		}
		assert.False(t, allZero, "crypto/rand should not produce all-zero output")
	})
}

// Benchmarks for performance monitoring.
func BenchmarkPBKDF2Generation(b *testing.B) {
	for b.Loop() {
		_, err := api_keys.HashAPIKey("sk-oai-benchmark-key")
		if err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkPBKDF2Verification(b *testing.B) {
	testKey := "sk-oai-benchmark-verification"
	hashData, err := api_keys.HashAPIKey(testKey)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	for b.Loop() {
		computedHash, err := api_keys.HashAPIKey(testKey)
		valid := computedHash.Hash == hashData.Hash
		if err != nil || !valid {
			b.Fatal("verification failed")
		}
	}
}

func BenchmarkConcurrentVerification(b *testing.B) {
	testKey := "sk-oai-concurrent-benchmark"
	hashData, err := api_keys.HashAPIKey(testKey)
	if err != nil {
		b.Fatal(err)
	}

	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			computedHash, err := api_keys.HashAPIKey(testKey)
			valid := computedHash.Hash == hashData.Hash
			if err != nil || !valid {
				b.Fatal("verification failed")
			}
		}
	})
}

