package com.neogo.neo.service

// PILAR XXVI 137.C.4 — BootReceiver: restart NeoMeshService after device boot.
//
// RECEIVE_BOOT_COMPLETED is declared in AndroidManifest.xml.
// The receiver is exported=false (only the OS can send this intent).
//
// Awaiting Android Studio compile validation.

import android.content.BroadcastReceiver
import android.content.Context
import android.content.Intent

class BootReceiver : BroadcastReceiver() {
    override fun onReceive(context: Context, intent: Intent) {
        if (intent.action != Intent.ACTION_BOOT_COMPLETED) return
        NeoMeshService.log("BootReceiver: device boot detected — starting NeoMeshService")
        NeoMeshService.start(context)
    }
}
