package com.neogo.neo

// PILAR XXVI 137.B.3 — Settings screen backed by EncryptedSharedPreferences.
//
// Keys bucket_url / access_key / secret_key / passphrase_hint / tailscale_auth_key
// are persisted with AES-256-GCM encryption via androidx.security.crypto so they
// survive process death without exposing secrets on a rooted device.
//
// Awaiting Android Studio compile validation.

import android.content.Context
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.foundation.rememberScrollState
import androidx.compose.foundation.verticalScroll
import androidx.compose.material3.Button
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.OutlinedTextField
import androidx.compose.material3.SnackbarHost
import androidx.compose.material3.SnackbarHostState
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.rememberCoroutineScope
import androidx.compose.runtime.setValue
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.text.input.PasswordVisualTransformation
import androidx.compose.ui.unit.dp
import androidx.security.crypto.EncryptedSharedPreferences
import androidx.security.crypto.MasterKey
import kotlinx.coroutines.launch

private const val PREFS_NAME = "neo_mesh_settings"
private const val KEY_BUCKET_URL = "bucket_url"
private const val KEY_ACCESS_KEY = "bucket_access_key"
private const val KEY_SECRET_KEY = "bucket_secret_key"
private const val KEY_PASSPHRASE_HINT = "passphrase_hint"
private const val KEY_TS_AUTH_KEY = "tailscale_auth_key"

fun openEncryptedPrefs(ctx: Context) =
    EncryptedSharedPreferences.create(
        ctx,
        PREFS_NAME,
        MasterKey.Builder(ctx).setKeyScheme(MasterKey.KeyScheme.AES256_GCM).build(),
        EncryptedSharedPreferences.PrefKeyEncryptionScheme.AES256_SIV,
        EncryptedSharedPreferences.PrefValueEncryptionScheme.AES256_GCM,
    )

@Composable
fun SettingsScreen(modifier: Modifier = Modifier) {
    val context = LocalContext.current
    val prefs = remember { openEncryptedPrefs(context) }
    val snack = remember { SnackbarHostState() }
    val scope = rememberCoroutineScope()

    var bucketUrl by remember { mutableStateOf(prefs.getString(KEY_BUCKET_URL, "") ?: "") }
    var accessKey by remember { mutableStateOf(prefs.getString(KEY_ACCESS_KEY, "") ?: "") }
    var secretKey by remember { mutableStateOf(prefs.getString(KEY_SECRET_KEY, "") ?: "") }
    var passphraseHint by remember { mutableStateOf(prefs.getString(KEY_PASSPHRASE_HINT, "") ?: "") }
    var tsAuthKey by remember { mutableStateOf(prefs.getString(KEY_TS_AUTH_KEY, "") ?: "") }

    Column(
        modifier = modifier
            .fillMaxSize()
            .verticalScroll(rememberScrollState())
            .padding(24.dp),
    ) {
        Text("Settings", style = MaterialTheme.typography.headlineMedium)
        Spacer(Modifier.height(16.dp))

        OutlinedTextField(
            value = bucketUrl,
            onValueChange = { bucketUrl = it },
            label = { Text("Bucket URL (R2 / S3)") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Spacer(Modifier.height(8.dp))
        OutlinedTextField(
            value = accessKey,
            onValueChange = { accessKey = it },
            label = { Text("Access Key") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Spacer(Modifier.height(8.dp))
        OutlinedTextField(
            value = secretKey,
            onValueChange = { secretKey = it },
            label = { Text("Secret Key") },
            visualTransformation = PasswordVisualTransformation(),
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Spacer(Modifier.height(8.dp))
        OutlinedTextField(
            value = passphraseHint,
            onValueChange = { passphraseHint = it },
            label = { Text("Passphrase Hint (shown on pull)") },
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Spacer(Modifier.height(8.dp))
        OutlinedTextField(
            value = tsAuthKey,
            onValueChange = { tsAuthKey = it },
            label = { Text("Tailscale Auth Key") },
            visualTransformation = PasswordVisualTransformation(),
            singleLine = true,
            modifier = Modifier.fillMaxWidth(),
        )
        Spacer(Modifier.height(16.dp))
        Button(
            onClick = {
                prefs.edit()
                    .putString(KEY_BUCKET_URL, bucketUrl.trim())
                    .putString(KEY_ACCESS_KEY, accessKey.trim())
                    .putString(KEY_SECRET_KEY, secretKey.trim())
                    .putString(KEY_PASSPHRASE_HINT, passphraseHint.trim())
                    .putString(KEY_TS_AUTH_KEY, tsAuthKey.trim())
                    .apply()
                scope.launch { snack.showSnackbar("Settings saved") }
            },
            modifier = Modifier.fillMaxWidth(),
        ) { Text("Save") }

        SnackbarHost(hostState = snack)
    }
}
