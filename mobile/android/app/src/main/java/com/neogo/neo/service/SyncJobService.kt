package com.neogo.neo.service

// PILAR XXVI 137.D.1 / 137.D.2 / 137.D.3 — SyncJobService: JobScheduler-based
// periodic sync for battery-optimised background runs.
//
// 137.D.1: JobService registered with JobScheduler; scheduled via scheduleSyncJob().
// 137.D.2: Constraints — NETWORK_TYPE_UNMETERED (Wi-Fi) + requires charging.
//          Matches the CHARGING_ONLY power policy but enforced by the OS scheduler.
// 137.D.3: Reports sync progress via NeoMeshService.log() so the Logs tab shows
//          when background jobs fired.
//
// Use alongside NeoMeshService: the foreground service handles explicit user
// requests; this job handles overnight / idle syncs.
//
// Awaiting Android Studio compile validation.

import android.app.job.JobInfo
import android.app.job.JobParameters
import android.app.job.JobScheduler
import android.app.job.JobService
import android.content.ComponentName
import android.content.Context
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.SupervisorJob
import kotlinx.coroutines.cancel
import kotlinx.coroutines.launch

private const val JOB_ID = 42
private const val SYNC_INTERVAL_MS = 4 * 60 * 60 * 1000L // 4 hours

class SyncJobService : JobService() {

    private val job = SupervisorJob()
    private val scope = CoroutineScope(Dispatchers.IO + job)

    override fun onStartJob(params: JobParameters): Boolean {
        NeoMeshService.log("SyncJobService: job started (id=${params.jobId})")
        scope.launch {
            try {
                NeoMeshService.log("SyncJobService: dispatching sync to NeoMeshService")
                NeoMeshService.requestSync(applicationContext)
                NeoMeshService.log("SyncJobService: sync requested")
                jobFinished(params, false) // false = don't reschedule on failure
            } catch (e: Exception) {
                NeoMeshService.log("SyncJobService: error — ${e.message}")
                jobFinished(params, true) // true = reschedule
            }
        }
        return true // work still running in coroutine
    }

    override fun onStopJob(params: JobParameters): Boolean {
        NeoMeshService.log("SyncJobService: job stopped early (id=${params.jobId})")
        scope.cancel()
        return true // reschedule
    }

    companion object {
        fun scheduleSyncJob(ctx: Context) {
            val scheduler = ctx.getSystemService(Context.JOB_SCHEDULER_SERVICE) as JobScheduler
            // [137.D.2] Requires unmetered network (Wi-Fi) + charging state.
            val info = JobInfo.Builder(JOB_ID, ComponentName(ctx, SyncJobService::class.java))
                .setRequiredNetworkType(JobInfo.NETWORK_TYPE_UNMETERED)
                .setRequiresCharging(true)
                .setPeriodic(SYNC_INTERVAL_MS)
                .setPersisted(true) // survives reboots (requires RECEIVE_BOOT_COMPLETED)
                .build()
            val result = scheduler.schedule(info)
            NeoMeshService.log("SyncJobService: scheduled result=$result interval=${SYNC_INTERVAL_MS / 3_600_000}h")
        }

        fun cancelSyncJob(ctx: Context) {
            val scheduler = ctx.getSystemService(Context.JOB_SCHEDULER_SERVICE) as JobScheduler
            scheduler.cancel(JOB_ID)
            NeoMeshService.log("SyncJobService: job cancelled")
        }
    }
}
