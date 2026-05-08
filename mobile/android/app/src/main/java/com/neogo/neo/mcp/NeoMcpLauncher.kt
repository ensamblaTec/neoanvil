package com.neogo.neo.mcp

// PILAR XXVI 137.F.1 — NeoMcpLauncher: bridge between the Android app and the
// neo-mcp binary bundled in libneo.aar.
//
// On Pixel devices with the Claude app installed, this launcher fires an explicit
// intent to register neo-mcp as a Custom Connector (137.F.2). On other devices it
// falls back to the local SSE transport so any MCP-compatible client can connect
// over the Tailscale mesh.
//
// The local SSE server binds to 127.0.0.1:<mcpPort> and is accessible from the
// device via localhost — no external network exposure. The Tailscale tsnet node
// (TailscaleManager) provides off-device access to the same port through its
// HTTP transport.
//
// 137.F.2 (Custom Connectors registration with claude.ai) is deferred until
// the Custom Connectors API is publicly available. This file provides the
// intent constants and a discovery helper so that feature can be wired in
// without modifying the launch flow.
//
// Awaiting Android Studio compile validation + libneo.aar linkage.

import android.content.Context
import android.content.Intent
import android.content.pm.PackageManager
import com.neogo.neo.service.NeoMeshService

private const val CLAUDE_PKG = "com.anthropic.claude"
private const val MCP_PORT = 9342
// Custom Connectors action (speculative — update when API is public, 137.F.2).
private const val ACTION_REGISTER_MCP = "com.anthropic.claude.REGISTER_MCP_CONNECTOR"

object NeoMcpLauncher {

    // isClaudeInstalled returns true when the Claude for Android app is present.
    fun isClaudeInstalled(ctx: Context): Boolean =
        runCatching {
            ctx.packageManager.getPackageInfo(CLAUDE_PKG, PackageManager.GET_ACTIVITIES)
            true
        }.getOrDefault(false)

    // startMcpServer starts the neo-mcp SSE server on the loopback interface.
    // When libneo.aar is linked (137.A.4) this calls into nativeStartMcpServer().
    // Until then it logs a stub entry.
    fun startMcpServer(ctx: Context) {
        NeoMeshService.log("NeoMcpLauncher: starting neo-mcp on 127.0.0.1:$MCP_PORT (stub)")
        // TODO(137.A.4): nativeStartMcpServer(MCP_PORT)
    }

    // registerWithClaude attempts to register neo-mcp as a Custom Connector
    // in the Claude app. No-ops gracefully if Claude is not installed or the
    // Custom Connectors API isn't available yet. [137.F.2]
    fun registerWithClaude(ctx: Context) {
        if (!isClaudeInstalled(ctx)) {
            NeoMeshService.log("NeoMcpLauncher: Claude app not installed — skipping registration")
            return
        }
        val intent = Intent(ACTION_REGISTER_MCP).apply {
            setPackage(CLAUDE_PKG)
            putExtra("mcp_url", "http://127.0.0.1:$MCP_PORT/mcp/sse")
            putExtra("mcp_name", "NeoAnvil (device)")
        }
        runCatching {
            ctx.sendBroadcast(intent)
            NeoMeshService.log("NeoMcpLauncher: registration intent sent to Claude")
        }.onFailure { e ->
            NeoMeshService.log("NeoMcpLauncher: registration failed — ${e.message}")
        }
    }

    // mcpSseUrl returns the local SSE endpoint for use in an MCP client config.
    fun mcpSseUrl(): String = "http://127.0.0.1:$MCP_PORT/mcp/sse"
}
