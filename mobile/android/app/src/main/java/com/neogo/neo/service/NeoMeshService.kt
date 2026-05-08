package com.neogo.neo.service

// PILAR XXVI 137.C.1 / 137.C.2 / 137.C.3 — NeoMeshService: persistent
// foreground service that owns the sync lifecycle.
//
// START_STICKY: the OS restarts the service automatically after it is killed
// for memory pressure. The WakeLock is re-acquired on restart.
//
// 137.C.2: FOREGROUND_SERVICE with a low-priority persistent notification so
//   the user sees the service is active without being intrusive.
// 137.C.3: WakeLock acquired only when syncing to prevent the CPU from sleeping
//   mid-transfer; released immediately after sync completes or fails.
//
// Awaiting Android Studio compile validation.

import android.app.NotificationChannel
import android.app.NotificationManager
import android.app.PendingIntent
import android.app.Service
import android.content.Context
import android.content.Intent
import android.os.IBinder
import android.os.PowerManager
import androidx.core.app.NotificationCompat
import com.neogo.neo.MainActivity
import com.neogo.neo.isSyncAllowed
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch
import java.text.SimpleDateFormat
import java.util.ArrayDeque
import java.util.Date
import java.util.Locale

private const val NOTIF_CHANNEL = "neo_mesh"
private const val NOTIF_ID = 1
private const val WAKELOCK_TAG = "neo_mesh:sync"
private const val LOG_RING_SIZE = 200
private const val ACTION_SYNC = "com.neogo.neo.ACTION_SYNC"

class NeoMeshService : Service() {

    private val job = SupervisorJob()
    private val scope = CoroutineScope(Dispatchers.IO + job)
    private var wakeLock: PowerManager.WakeLock? = null

    override fun onCreate() {
        super.onCreate()
        createNotifChannel()
        startForeground(NOTIF_ID, buildNotification("Neo-Mesh running"))
        log("NeoMeshService started")
    }

    override fun onStartCommand(intent: Intent?, flags: Int, startId: Int): Int {
        if (intent?.action == ACTION_SYNC) {
            scope.launch { runSync() }
        }
        return START_STICKY
    }

    override fun onBind(intent: Intent?): IBinder? = null

    override fun onDestroy() {
        scope.cancel()
        releaseWakeLock()
        super.onDestroy()
    }

    // ── sync logic ────────────────────────────────────────────────────────────

    private suspend fun runSync() {
        if (!isSyncAllowed(this)) {
            log("Sync skipped — power policy")
            return
        }
        acquireWakeLock()
        try {
            log("Sync started")
            // The actual push/pull through the BrainStore drivers calls into the
            // native library (libneo.aar, 137.A.4). Until that is linked,
            // this stub records the event so the Logs tab shows activity.
            kotlinx.coroutines.delay(500)
            log("Sync complete (stub — libneo.aar not linked yet)")
        } catch (e: Exception) {
            log("Sync error: ${e.message}")
        } finally {
            releaseWakeLock()
        }
    }

    // ── WakeLock ──────────────────────────────────────────────────────────────

    private fun acquireWakeLock() {
        val pm = getSystemService(POWER_SERVICE) as PowerManager
        wakeLock = pm.newWakeLock(PowerManager.PARTIAL_WAKE_LOCK, WAKELOCK_TAG).apply {
            acquire(10 * 60 * 1000L) // cap: 10 min
        }
    }

    private fun releaseWakeLock() {
        wakeLock?.let { if (it.isHeld) it.release() }
        wakeLock = null
    }

    // ── notification ─────────────────────────────────────────────────────────

    private fun createNotifChannel() {
        val nm = getSystemService(NOTIFICATION_SERVICE) as NotificationManager
        if (nm.getNotificationChannel(NOTIF_CHANNEL) != null) return
        nm.createNotificationChannel(
            NotificationChannel(NOTIF_CHANNEL, "Neo-Mesh Sync", NotificationManager.IMPORTANCE_LOW)
                .apply { description = "Background sync service" }
        )
    }

    private fun buildNotification(text: String) =
        NotificationCompat.Builder(this, NOTIF_CHANNEL)
            .setSmallIcon(android.R.drawable.ic_popup_sync)
            .setContentTitle("Neo-Mesh")
            .setContentText(text)
            .setPriority(NotificationCompat.PRIORITY_LOW)
            .setOngoing(true)
            .setContentIntent(
                PendingIntent.getActivity(
                    this, 0,
                    Intent(this, MainActivity::class.java),
                    PendingIntent.FLAG_IMMUTABLE,
                )
            )
            .build()

    // ── companion / static helpers ────────────────────────────────────────────

    companion object {
        // In-memory log ring surfaced by the Logs tab. [137.C.3]
        val logRing: ArrayDeque<String> = ArrayDeque(LOG_RING_SIZE)
        private val fmt = SimpleDateFormat("HH:mm:ss", Locale.getDefault())

        fun log(msg: String) {
            synchronized(logRing) {
                if (logRing.size >= LOG_RING_SIZE) logRing.pollFirst()
                logRing.addLast("${fmt.format(Date())}  $msg")
            }
        }

        fun start(ctx: Context) {
            ctx.startForegroundService(Intent(ctx, NeoMeshService::class.java))
        }

        fun requestSync(ctx: Context) {
            ctx.startForegroundService(
                Intent(ctx, NeoMeshService::class.java).setAction(ACTION_SYNC)
            )
        }
    }
}
