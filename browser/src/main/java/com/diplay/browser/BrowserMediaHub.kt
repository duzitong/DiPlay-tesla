package com.diplay.browser

import com.diplay.browser.rtc.BrowserVideoOutput
import com.diplay.browser.rtc.RtcRecoveryPolicy
import org.json.JSONObject
import java.security.MessageDigest
import java.util.UUID

/** Composition seam for one authenticated media viewer; the core and proxy do not know it. */
class BrowserMediaHub(private val record: (String) -> Unit = {}) {
    val audio = BrowserAudioOutput().apply { diagnostic = record }
    private val lock = Any()
    private var owner: Any? = null
    private var send: ((JSONObject) -> Boolean)? = null
    private var rtc = false
    private var webcodecs = false
    private var session: String? = null
    private data class VideoLink(val owner: Any, val control: (JSONObject) -> Boolean,
        val send: (ByteArray) -> Boolean, val stop: () -> Unit)
    private var videoLink: VideoLink? = null
    private var recoveryGraceMs = RtcRecoveryPolicy.BALANCED.graceMs
    @Volatile private var video: BrowserVideoOutput? = null
    val transport get() = video?.transport ?: "waiting"
    val videoCodec get() = video?.codecName
    val viewerAttached: Boolean get() = synchronized(lock) { owner != null }
    internal val webCodecsSelected: Boolean get() = synchronized(lock) { webcodecs }
    fun video(output: BrowserVideoOutput?) = synchronized(lock) {
        video?.detach()
        video = output
        output?.eventLogger = ::note
        send?.let { if (webcodecs) output?.attachWebCodecs() else output?.attach(rtc, recoveryGraceMs, it) }
        attachVideoOutput()
    }
    fun attach(owner: Any, rtc: Boolean, send: (JSONObject) -> Boolean, audio: (ByteArray, Long) -> Boolean,
        recoveryGraceMs: Long = RtcRecoveryPolicy.BALANCED.graceMs, webcodecs: Boolean = false): Boolean = synchronized(lock) {
        if (this.owner != null) return false
        this.owner = owner; this.send = send; this.rtc = rtc
        note("Browser media attached rtc=$rtc")
        this.webcodecs = webcodecs; session = UUID.randomUUID().toString()
        this.recoveryGraceMs = RtcRecoveryPolicy.fromMillis(recoveryGraceMs).graceMs
        this.audio.attach(owner, audio, send)
        if (webcodecs) video?.attachWebCodecs() else video?.attach(rtc, this.recoveryGraceMs, send)
        true
    }
    fun videoSession(owner: Any): String? = synchronized(lock) { if (this.owner === owner) session else null }
    internal fun attachVideo(owner: Any, session: String, control: (JSONObject) -> Boolean,
        send: (ByteArray) -> Boolean, stop: () -> Unit): Boolean = synchronized(lock) {
        val expected = this.session ?: return false
        if (!webcodecs || videoLink != null ||
            !MessageDigest.isEqual(expected.toByteArray(), session.toByteArray())) return false
        videoLink = VideoLink(owner, control, send, stop)
        attachVideoOutput()
        true
    }
    private fun attachVideoOutput() {
        val link = videoLink ?: return
        val ownerSend = send
        video?.h264?.attach(link.owner, { data ->
            // Teardown discards the video TCP backlog, including queued error messages.
            if (data.optString("type") == "video-error") ownerSend?.invoke(data)
            link.control(data)
        }, link.send, link.stop)
    }
    internal fun videoCommand(owner: Any, data: JSONObject) = synchronized(lock) {
        if (videoLink?.owner === owner) video?.h264?.command(owner, data)
    }
    internal fun detachVideo(owner: Any) = synchronized(lock) {
        if (videoLink?.owner !== owner) return
        video?.h264?.detach(owner); videoLink = null
    }
    fun command(owner: Any, data: JSONObject) = synchronized(lock) {
        if (this.owner !== owner) return
        when (data.getString("type")) {
            "audio-ready" -> audio.ready(owner, data.getBoolean("playback"))
            "rtc-answer", "rtc-ready", "rtc-feedback", "rtc-fallback" -> video?.command(data)
        }
    }
    fun detach(owner: Any) = synchronized(lock) {
        if (this.owner !== owner) return
        videoLink?.stop?.invoke(); videoLink = null; session = null; webcodecs = false
        audio.detach(owner); video?.detach(); this.owner = null; send = null
        note("Browser media detached")
    }
    internal fun resetAudio(owner: Any) = synchronized(lock) {
        if (this.owner === owner) audio.reset(owner)
    }
    internal fun note(message: String) { runCatching { record(message) } }
}
