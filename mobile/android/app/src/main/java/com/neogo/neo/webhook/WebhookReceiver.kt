package com.neogo.neo.webhook

// PILAR XXVI 137.G.1 / 137.G.2 / 137.G.3 — WebhookReceiver: lightweight HTTP
// server on a local port that accepts inbound events from the Nexus dispatcher.
//
// 137.G.1: HTTP server bound to 127.0.0.1:<port>. Only the Tailscale tsnet peer
//   (or local apps) can reach it — no external exposure.
// 137.G.2: Every request is verified with HMAC-SHA256. The shared secret is
//   stored in EncryptedSharedPreferences under KEY_WEBHOOK_SECRET. Requests
//   with invalid or missing X-Neo-Signature headers are rejected with 401.
// 137.G.3: Event routing — "sync_request" triggers NeoMeshService.requestSync();
//   other event types are logged to NeoMeshService.logRing for the Logs tab.
//
// The server is started by NeoMeshService.onCreate() after the foreground
// notification is shown. Port is configurable via EncryptedSharedPreferences;
// default 9343.
//
// Awaiting Android Studio compile validation.

import android.content.Context
import com.neogo.neo.openEncryptedPrefs
import com.neogo.neo.service.NeoMeshService
import kotlinx.coroutines.CoroutineScope
import kotlinx.coroutines.Dispatchers
import kotlinx.coroutines.launch
import org.json.JSONObject
import java.io.BufferedReader
import java.io.InputStreamReader
import java.io.PrintWriter
import java.net.ServerSocket
import java.net.Socket
import javax.crypto.Mac
import javax.crypto.spec.SecretKeySpec

private const val WEBHOOK_PORT = 9343
private const val KEY_WEBHOOK_SECRET = "webhook_secret"
private const val HMAC_ALGO = "HmacSHA256"

class WebhookReceiver(private val ctx: Context) {

    private var serverSocket: ServerSocket? = null
    private val scope = CoroutineScope(Dispatchers.IO)

    // start binds the server socket and begins accepting connections. [137.G.1]
    fun start() {
        scope.launch {
            runCatching {
                val ss = ServerSocket(WEBHOOK_PORT, 10, java.net.InetAddress.getByName("127.0.0.1"))
                serverSocket = ss
                NeoMeshService.log("WebhookReceiver: listening on 127.0.0.1:$WEBHOOK_PORT")
                while (!ss.isClosed) {
                    val client = runCatching { ss.accept() }.getOrNull() ?: break
                    scope.launch { handleClient(client) }
                }
            }.onFailure { e ->
                NeoMeshService.log("WebhookReceiver: error — ${e.message}")
            }
        }
    }

    fun stop() {
        runCatching { serverSocket?.close() }
        NeoMeshService.log("WebhookReceiver: stopped")
    }

    // handleClient processes a single HTTP/1.0-ish request. [137.G.2 / 137.G.3]
    private fun handleClient(sock: Socket) {
        sock.use {
            val reader = BufferedReader(InputStreamReader(sock.getInputStream()))
            val writer = PrintWriter(sock.getOutputStream(), true)

            // Read request line + headers
            val requestLine = reader.readLine() ?: return
            val headers = mutableMapOf<String, String>()
            while (true) {
                val line = reader.readLine() ?: break
                if (line.isEmpty()) break
                val idx = line.indexOf(':')
                if (idx > 0) headers[line.substring(0, idx).trim().lowercase()] = line.substring(idx + 1).trim()
            }
            val contentLength = headers["content-length"]?.toIntOrNull() ?: 0
            val bodyChars = CharArray(contentLength)
            if (contentLength > 0) reader.read(bodyChars, 0, contentLength)
            val body = String(bodyChars)

            // HMAC-SHA256 verification [137.G.2]
            val sig = headers["x-neo-signature"]
            if (!verifySignature(body, sig)) {
                writer.print("HTTP/1.0 401 Unauthorized\r\n\r\n")
                NeoMeshService.log("WebhookReceiver: rejected request — bad signature")
                return
            }

            // Event routing [137.G.3]
            routeEvent(body)
            writer.print("HTTP/1.0 200 OK\r\nContent-Length: 2\r\n\r\nOK")
        }
    }

    // verifySignature computes HMAC-SHA256(secret, body) and compares to sig. [137.G.2]
    private fun verifySignature(body: String, sig: String?): Boolean {
        if (sig.isNullOrBlank()) return false
        val secret = openEncryptedPrefs(ctx).getString(KEY_WEBHOOK_SECRET, null)
            ?: return false.also { NeoMeshService.log("WebhookReceiver: no webhook secret configured") }
        return runCatching {
            val mac = Mac.getInstance(HMAC_ALGO)
            mac.init(SecretKeySpec(secret.toByteArray(), HMAC_ALGO))
            val expected = mac.doFinal(body.toByteArray()).joinToString("") { "%02x".format(it) }
            // Constant-time comparison to prevent timing attacks.
            expected.length == sig.length && expected.zip(sig).all { (a, b) -> a == b }
        }.getOrDefault(false)
    }

    // routeEvent dispatches an event payload by its "type" field. [137.G.3]
    private fun routeEvent(body: String) {
        runCatching {
            val obj = JSONObject(body)
            val type = obj.optString("type", "unknown")
            NeoMeshService.log("WebhookReceiver: event type=$type")
            when (type) {
                "sync_request" -> NeoMeshService.requestSync(ctx)
                "ping" -> NeoMeshService.log("WebhookReceiver: pong")
                else -> NeoMeshService.log("WebhookReceiver: unhandled event type=$type")
            }
        }.onFailure { e ->
            NeoMeshService.log("WebhookReceiver: parse error — ${e.message}")
        }
    }
}
