package com.diplay.browser

import fi.iki.elonen.NanoHTTPD.IHTTPSession
import fi.iki.elonen.NanoWSD.WebSocket
import fi.iki.elonen.NanoWSD.WebSocketFrame
import org.json.JSONObject
import java.io.IOException
import java.security.MessageDigest
import java.util.concurrent.ArrayBlockingQueue
import java.util.concurrent.atomic.AtomicBoolean

/** This link borrows the media owner's lease; its failure never detaches PCM/signaling. */
internal class BrowserVideoSocket(
    handshake: IHTTPSession,
    private val token: String,
    private val media: BrowserMediaHub,
    private val ended: (BrowserVideoSocket) -> Unit,
) : WebSocket(object : IHTTPSession by handshake {
    override fun getInputStream() = BoundedWebSocketInput(handshake.inputStream)
}) {
    private val queue = ArrayBlockingQueue<WebSocketFrame>(8)
    private val closed = AtomicBoolean()
    @Volatile private var authenticated = false
    @Volatile private var seen = System.nanoTime()
    @Volatile private var failed = false
    private val writer = Thread({
        try { while (!closed.get()) super.sendFrame(queue.take()) }
        catch (_: InterruptedException) { }
        catch (_: IOException) { requestDisconnect() }
    }, "browser-video-writer").apply { isDaemon = true }
    override fun onOpen() { writer.start() }
    private fun enqueue(frame: WebSocketFrame): Boolean {
        if (closed.get() || failed) return false
        if (queue.offer(frame)) return true
        requestDisconnect(); return false
    }
    private fun sendControl(data: JSONObject) = enqueue(WebSocketFrame(WebSocketFrame.OpCode.Text, true, data.toString()))
    private fun sendVideo(bytes: ByteArray) = enqueue(WebSocketFrame(WebSocketFrame.OpCode.Binary, true, bytes))
    override fun sendFrame(frame: WebSocketFrame) { enqueue(frame) }
    override fun onMessage(message: WebSocketFrame) {
        if (closed.get()) return
        try {
            require(message.opCode == WebSocketFrame.OpCode.Text && message.binaryPayload.size <= 2048)
            val data = JSONObject(message.textPayload)
            val type = data.getString("type")
            if (!authenticated) {
                require(type == "hello" && MessageDigest.isEqual(data.getString("token").toByteArray(), token.toByteArray()))
                require(media.attachVideo(this, data.getString("session"), ::sendControl, ::sendVideo, ::requestDisconnect))
                authenticated = true
                sendControl(JSONObject().put("type", "video-welcome"))
            } else when (type) {
                "ping" -> sendControl(JSONObject().put("type", "pong"))
                "video-ready", "video-ack" -> {
                    require(data.getLong("generation") in 1..9_007_199_254_740_991L)
                    if (type == "video-ack") require(data.getLong("sequence") in 0..9_007_199_254_740_991L)
                    media.videoCommand(this, data)
                }
                else -> throw IllegalArgumentException()
            }
            seen = System.nanoTime()
        } catch (_: Exception) { disconnect() }
    }
    fun expire() {
        if (failed || System.nanoTime() - seen > (if (authenticated) 5_000_000_000L else 3_000_000_000L)) disconnect()
    }
    private fun requestDisconnect() {
        // Output/hub callbacks must not acquire the hub lock from the output lock.
        failed = true; queue.clear(); writer.interrupt()
        runCatching { handshakeRequest.inputStream.close() }
    }
    fun disconnect() {
        if (!closed.compareAndSet(false, true)) return
        media.detachVideo(this); queue.clear(); writer.interrupt()
        runCatching { handshakeRequest.inputStream.close() }
        ended(this)
    }
    override fun close(code: WebSocketFrame.CloseCode?, reason: String?, initiatedByRemote: Boolean) = disconnect()
    override fun onClose(code: WebSocketFrame.CloseCode?, reason: String?, initiatedByRemote: Boolean) = disconnect()
    override fun onException(exception: IOException) = disconnect()
    override fun onPong(pong: WebSocketFrame) { seen = System.nanoTime() }
}
