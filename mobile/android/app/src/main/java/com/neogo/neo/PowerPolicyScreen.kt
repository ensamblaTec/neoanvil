package com.neogo.neo

// PILAR XXVI 137.B.4 — Power Policy screen.
//
// Three policies:
//   CHARGING_ONLY  — sync only when connected to a charger (default, battery-safe)
//   WIFI_ONLY      — sync only on Wi-Fi, regardless of charging state
//   ALWAYS         — sync whenever network is available (use for desktop-pinned nodes)
//
// Persisted in EncryptedSharedPreferences. NetworkType detection via
// ConnectivityManager tells the Sync tab whether the current policy allows a sync.
//
// Awaiting Android Studio compile validation.

import android.content.Context
import android.net.ConnectivityManager
import android.net.NetworkCapabilities
import android.os.BatteryManager
import androidx.compose.foundation.layout.Column
import androidx.compose.foundation.layout.Row
import androidx.compose.foundation.layout.Spacer
import androidx.compose.foundation.layout.fillMaxSize
import androidx.compose.foundation.layout.fillMaxWidth
import androidx.compose.foundation.layout.height
import androidx.compose.foundation.layout.padding
import androidx.compose.material3.MaterialTheme
import androidx.compose.material3.RadioButton
import androidx.compose.material3.Text
import androidx.compose.runtime.Composable
import androidx.compose.runtime.getValue
import androidx.compose.runtime.mutableStateOf
import androidx.compose.runtime.remember
import androidx.compose.runtime.setValue
import androidx.compose.ui.Alignment
import androidx.compose.ui.Modifier
import androidx.compose.ui.platform.LocalContext
import androidx.compose.ui.unit.dp

private const val KEY_POWER_POLICY = "power_policy"

enum class PowerPolicy(val label: String) {
    CHARGING_ONLY("Charging only (recommended)"),
    WIFI_ONLY("Wi-Fi only"),
    ALWAYS("Always (drain warning)"),
}

fun readPowerPolicy(ctx: Context): PowerPolicy {
    val prefs = openEncryptedPrefs(ctx)
    val raw = prefs.getString(KEY_POWER_POLICY, PowerPolicy.CHARGING_ONLY.name)
    return runCatching { PowerPolicy.valueOf(raw!!) }.getOrDefault(PowerPolicy.CHARGING_ONLY)
}

fun writePowerPolicy(ctx: Context, policy: PowerPolicy) {
    openEncryptedPrefs(ctx).edit().putString(KEY_POWER_POLICY, policy.name).apply()
}

fun isSyncAllowed(ctx: Context): Boolean {
    val policy = readPowerPolicy(ctx)
    val cm = ctx.getSystemService(Context.CONNECTIVITY_SERVICE) as ConnectivityManager
    val caps = cm.getNetworkCapabilities(cm.activeNetwork) ?: return false
    val onWifi = caps.hasTransport(NetworkCapabilities.TRANSPORT_WIFI)
    return when (policy) {
        PowerPolicy.ALWAYS -> true
        PowerPolicy.WIFI_ONLY -> onWifi
        PowerPolicy.CHARGING_ONLY -> {
            val bm = ctx.getSystemService(Context.BATTERY_SERVICE) as BatteryManager
            bm.isCharging
        }
    }
}

@Composable
fun PowerPolicyScreen(modifier: Modifier = Modifier) {
    val context = LocalContext.current
    var selected by remember { mutableStateOf(readPowerPolicy(context)) }

    Column(
        modifier = modifier
            .fillMaxSize()
            .padding(24.dp),
    ) {
        Text("Power Policy", style = MaterialTheme.typography.headlineMedium)
        Spacer(Modifier.height(16.dp))
        PowerPolicy.entries.forEach { policy ->
            Row(
                modifier = Modifier
                    .fillMaxWidth()
                    .padding(vertical = 4.dp),
                verticalAlignment = Alignment.CenterVertically,
            ) {
                RadioButton(
                    selected = selected == policy,
                    onClick = {
                        selected = policy
                        writePowerPolicy(context, policy)
                    },
                )
                Text(
                    text = policy.label,
                    style = MaterialTheme.typography.bodyLarge,
                    modifier = Modifier.padding(start = 8.dp),
                )
            }
        }
    }
}
