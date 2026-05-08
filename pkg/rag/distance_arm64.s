// distance_arm64.s — ARM64 NEON cosine similarity kernel. [Épica 305.C]
//
// Implements cosineNEON(a, b []float32) float32 using 128-bit NEON registers.
// Three VFMLA accumulators: V0=dot, V1=na, V2=nb (zeroed with VEOR).
// Horizontal reduction: VEXT rotates lanes → FADDS sums each lane to scalar.
//   In ARM64, Fn and Vn.S[0] alias the same register, so F0=V0.S[0] directly.
// Tail: n%4 residual elements handled with scalar FMULS/FADDS.
//
// Frame: 0 bytes local, 52 bytes args+ret.
//   a: 0(FP) ptr, 8(FP) len, 16(FP) cap
//   b: 24(FP) ptr, 32(FP) len, 40(FP) cap
//   ret: 48(FP)

#include "textflag.h"

TEXT ·cosineNEON(SB), NOSPLIT, $0-52
	MOVD a_ptr+0(FP), R0    // R0 = &a[0]
	MOVD a_len+8(FP), R2    // R2 = len(a)
	MOVD b_ptr+24(FP), R1   // R1 = &b[0]

	CBZ R2, returnzero

	// Zero SIMD accumulators
	VEOR V0.B16, V0.B16, V0.B16
	VEOR V1.B16, V1.B16, V1.B16
	VEOR V2.B16, V2.B16, V2.B16

	// SIMD loop — 4 float32 per iteration
	// VFMLA Vm.T, Vn.T, Vd.T  →  Vd[i] += Vn[i] * Vm[i]
	LSR $2, R2, R3
	CBZ R3, reduce

loop4:
	VLD1.P 16(R0), [V3.S4]
	VLD1.P 16(R1), [V4.S4]
	VFMLA V4.S4, V3.S4, V0.S4   // V0 += V3 * V4  (dot)
	VFMLA V3.S4, V3.S4, V1.S4   // V1 += V3 * V3  (na)
	VFMLA V4.S4, V4.S4, V2.S4   // V2 += V4 * V4  (nb)
	SUBS $1, R3, R3
	BNE loop4

reduce:
	// Horizontal sum of each 4-lane accumulator to scalar.
	// Fn and Vn.S[0] alias the same register; VEXT rotates by N bytes so
	// lane N/4 becomes lane 0, then FADDS accumulates into the scalar result.
	// VEXT $n, Vm.B16, Vm.B16, V3.B16  →  V3 = {Vm[n..15], Vm[0..n-1]}
	// ∴ V3.S[0] = F3 = Vm.S[n/4].

	// Horizontal sum via VEXT rotation: lane N becomes lane 0 after VEXT $4N.
	// F0=V0.S[0], F1=V1.S[0], F2=V2.S[0] via the Fn↔Vn.S[0] alias.

	// V0 → F5 (dot)
	VEXT $4, V0.B16, V0.B16, V3.B16   // F3 = V0.S[1]
	FADDS F0, F3, F5                    // F5 = V0.S[0] + V0.S[1]
	VEXT $8, V0.B16, V0.B16, V3.B16   // F3 = V0.S[2]
	FADDS F3, F5, F5                    // F5 += V0.S[2]
	VEXT $12, V0.B16, V0.B16, V3.B16  // F3 = V0.S[3]
	FADDS F3, F5, F5                    // F5 += V0.S[3]

	// V1 → F6 (na)
	VEXT $4, V1.B16, V1.B16, V3.B16
	FADDS F1, F3, F6                    // F6 = V1.S[0] + V1.S[1]
	VEXT $8, V1.B16, V1.B16, V3.B16
	FADDS F3, F6, F6                    // F6 += V1.S[2]
	VEXT $12, V1.B16, V1.B16, V3.B16
	FADDS F3, F6, F6                    // F6 += V1.S[3]

	// V2 → F7 (nb)
	VEXT $4, V2.B16, V2.B16, V3.B16
	FADDS F2, F3, F7                    // F7 = V2.S[0] + V2.S[1]
	VEXT $8, V2.B16, V2.B16, V3.B16
	FADDS F3, F7, F7                    // F7 += V2.S[2]
	VEXT $12, V2.B16, V2.B16, V3.B16
	FADDS F3, F7, F7                    // F7 += V2.S[3]

tail:
	AND $3, R2, R3
	CBZ R3, compute

tailloop:
	FMOVS (R0), F8               // F8 = a[i]
	FMOVS (R1), F9               // F9 = b[i]
	FMULS F9, F8, F10
	FADDS F10, F5, F5            // dot += a * b
	FMULS F8, F8, F10
	FADDS F10, F6, F6            // na  += a * a
	FMULS F9, F9, F10
	FADDS F10, F7, F7            // nb  += b * b
	ADD $4, R0, R0
	ADD $4, R1, R1
	SUBS $1, R3, R3
	BNE tailloop

compute:
	// result = dot / sqrt(na * nb)
	// FDIVS Sm, Sn, Sd  →  Sd = Sn / Sm
	FMULS F7, F6, F8             // F8 = na * nb
	FSQRTS F8, F8                // F8 = sqrt(na * nb)
	FDIVS F8, F5, F0             // F0 = dot / sqrt(na * nb)
	FMOVS F0, ret+48(FP)
	RET

returnzero:
	MOVW ZR, ret+48(FP)          // 0x00000000 = +0.0f
	RET
