package com.neogo.neo.tailscale

// PILAR XXVI 137.E.1 / 137.E.2 — TailscaleManager: tsnet Android compatibility
// shim and auth key storage.
//
// 137.E.1: tsnet compatibility note.
//   tailscale.com/tsnet targets Go's standard net stack. On Android, the
//   gomobile-bridged binary (libneo.aar) must be compiled with CGO_ENABLED=0
//   and the `ts_netstack` build tag to use the userspace WireGuard netstack
//   (tun/netstack) instead of the kernel tun device. The result is a single
//   .so that carries its own tailnet node without requiring the Tailscale app.
//   This file is the Kotlin side of that bridge; the Go side lives in
//   pkg/brain/storage/tsnet.go (TsnetStore/TsnetServer).
//
// 137.E.2: Auth key persisted in the Android Keystore (hardware-backed on
//   Pixel 6+). The key itself lives in EncryptedSharedPreferences under
//   KEY_TS_AUTH_KEY (see SettingsScreen.kt). TailscaleManager reads it at
//   sync-time and passes it to the native library via JNI.
//
// Awaiting Android Studio compile validation + libneo.aar linkage.

import android.content.Context
import android.security.keystore.KeyGenParameterSpec
import android.security.keystore.KeyProperties
import com.neogo.neo.SettingsScreen
import com.neogo.neo.openEncryptedPrefs
import com.neogo.neo.service.NeoMeshService
import java.security.KeyPairGenerator
import java.security.KeyStore

private const val KEYSTORE_PROVIDER = "AndroidKeyStore"
private const val KEY_ALIAS_TS = "neo_tsnet_key"

object TailscaleManager {

    // readAuthKey returns the Tailscale auth key from EncryptedSharedPreferences.
    // Returns null if the user hasn't configured one yet.
    fun readAuthKey(ctx: Context): String? {
        val prefs = openEncryptedPrefs(ctx)
        val key = prefs.getString("tailscale_auth_key", null)
        return if (key.isNullOrBlank()) null else key
    }

    // generateNodeKey creates a deterministic Ed25519 key pair for this device
    // in the Android Keystore. Used for peer identity when the tsnet node
    // doesn't use an auth key (rekey flows, 137.E.2 follow-up).
    fun ensureNodeKey(): Boolean {
        return runCatching {
            val ks = KeyStore.getInstance(KEYSTORE_PROVIDER).also { it.load(null) }
            if (ks.containsAlias(KEY_ALIAS_TS)) return@runCatching true
            val spec = KeyGenParameterSpec.Builder(
                KEY_ALIAS_TS,
                KeyProperties.PURPOSE_SIGN or KeyProperties.PURPOSE_VERIFY,
            )
                .setAlgorithmParameterSpec(java.security.spec.ECGenParameterSpec("secp256r1"))
                .setDigests(KeyProperties.DIGEST_SHA256)
                .setUserAuthenticationRequired(false)
                .build()
            KeyPairGenerator.getInstance(KeyProperties.KEY_ALGORITHM_EC, KEYSTORE_PROVIDER)
                .apply { initialize(spec) }
                .generateKeyPair()
            NeoMeshService.log("TailscaleManager: node key generated in Keystore")
            true
        }.getOrElse { e ->
            NeoMeshService.log("TailscaleManager: key gen error — ${e.message}")
            false
        }
    }

    // startNode is the JNI bridge stub. When libneo.aar is linked (137.A.4),
    // this will call into Go's TsnetStore bootstrap with the auth key + state dir.
    fun startNode(ctx: Context, stateDir: String): Boolean {
        val authKey = readAuthKey(ctx) ?: run {
            NeoMeshService.log("TailscaleManager: no auth key — configure in Settings")
            return false
        }
        NeoMeshService.log("TailscaleManager: startNode stateDir=$stateDir (JNI stub)")
        // TODO(137.A.4): call nativeStartTsnet(authKey, stateDir) once libneo.aar is linked.
        return true
    }
}
