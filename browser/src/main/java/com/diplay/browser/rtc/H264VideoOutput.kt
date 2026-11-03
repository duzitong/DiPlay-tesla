package com.diplay.browser.rtc

import com.shilapi.xcertplay.airplay.VideoCodec
import org.json.JSONObject
import java.io.Closeable
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.util.concurrent.Executors
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicLong

/** Dedicated bounded video writer; unsent gaps resume only at a fresh keyframe. */
internal class H264VideoOutput(
    private val requestKey: () -> Unit,
    private val diagnostic: (String) -> Unit,
    private val clock: () -> Long = System::nanoTime,
) : Closeable {
    private data class Pending(val sequence: Long, val bytes: Int, val at: Long)
    private val worker = Executors.newSingleThreadScheduledExecutor {
        Thread(it, "browser-h264").apply { isDaemon = true }
    }
    private val queue = RtcFrameQueue()
    private val pending = ArrayDeque<Pending>()
    private var owner: Any? = null
    private var control: ((JSONObject) -> Boolean)? = null
    private var send: ((ByteArray) -> Boolean)? = null
    private var disconnect: (() -> Unit)? = null
    private var config: ByteArray? = null
    private var codec = VideoCodec.H264
    private var parameters = ByteArray(0)
    private var generation = 0L
    private var sequence = 0L
    private var acknowledged = 0L
    private var origin: Long? = null
    private var timestamp = -1L
    private var ready = false
    private var configuringAt: Long? = null
    private var reportedAt = 0L
    private var queueLossReported = false
    private var queueRecoveryAt: Long? = null
    private var queueRecoveryAfterSequence = 0L
    private var closed = false
    @Volatile var displayed = false; private set
    init { worker.scheduleAtFixedRate(::tick, 10, 10, TimeUnit.MILLISECONDS) }

    @Synchronized fun attach(owner: Any, control: (JSONObject) -> Boolean,
        send: (ByteArray) -> Boolean, disconnect: () -> Unit) {
        detach()
        this.owner = owner; this.control = control; this.send = send; this.disconnect = disconnect
        configure()
    }
    @Synchronized fun detach(owner: Any? = this.owner) {
        if (this.owner !== owner) return
        val stop = disconnect
        this.owner = null; control = null; send = null; disconnect = null
        ready = false; displayed = false; configuringAt = null
        queue.clear(); pending.clear()
        stop?.invoke()
    }
    @Synchronized fun codec(value: VideoCodec) {
        if (codec == value) return
        codec = value; config = null; configure()
    }
    @Synchronized fun config(bytes: ByteArray?) {
        if (config?.contentEquals(bytes ?: ByteArray(0)) == true) return
        config = bytes?.copyOf(); configure()
    }
    private fun configure() {
        ready = false; displayed = false; queue.reset(); pending.clear()
        sequence = 0; acknowledged = 0; origin = null; timestamp = -1
        reportedAt = clock()
        queueLossReported = false
        queueRecoveryAt = null
        queueRecoveryAfterSequence = 0
        generation = GENERATIONS.incrementAndGet()
        configuringAt = null
        if (owner == null) return
        if (codec != VideoCodec.H264) { fail("h264-required", false); return }
        val bytes = config ?: run {
            if (control?.invoke(JSONObject().put("type", "video-reset").put("generation", generation)) != true)
                fail("video-send-failed", true)
            return
        }
        val video = try { RtcVideoConfig.parse(codec, bytes) }
            catch (_: IllegalArgumentException) { fail("invalid-video-config", false); return }
        parameters = video.parameters
        configuringAt = clock()
        if (control?.invoke(JSONObject().put("type", "video-config").put("generation", generation)
                .put("codec", video.webCodecsCodec)) != true) fail("video-send-failed", true)
    }
    @Synchronized fun command(owner: Any, data: JSONObject) {
        if (this.owner !== owner || data.optLong("generation", -1) != generation) return
        when (data.getString("type")) {
            "video-ready" -> {
                if (!ready) { ready = true; configuringAt = null; requestKey() }
            }
            "video-ack" -> {
                val consumed = data.getLong("sequence")
                require(consumed in acknowledged..sequence)
                acknowledged = consumed
                while (pending.firstOrNull()?.let { it.sequence <= consumed } == true) pending.removeFirst()
                if (consumed > queueRecoveryAfterSequence) queueRecoveryAt = null
                if (!displayed && consumed > 0) { displayed = true; diagnostic("first frame rendered") }
            }
        }
    }
    @Synchronized fun frame(bytes: ByteArray, nanos: Long) {
        if (closed || !ready || owner == null) return
        val frame = RtcAccessUnit.inspect(bytes) ?: run { resetChain(); return }
        val prefix = if (frame.key) parameters else ByteArray(0)
        if (frame.size.toLong() + prefix.size > RtcFrameQueue.MAX_BYTES) { resetChain(); return }
        val now = clock()
        if (queueRecoveryAt != null && !queue.sourceActive(now)) queueRecoveryAt = now
        queue.offer(frame.size, nanos, frame.key, now, prefix.size) { frame.copyAnnexB(prefix) }
        noteQueueLoss()
        if (queue.needsKey) requestKey()
    }
    private fun noteQueueLoss() {
        if (queue.takeSendFailure() == null) return
        if (queueRecoveryAt == null) queueRecoveryAt = clock()
        queueRecoveryAfterSequence = sequence
        // Only unsent frames were dropped. Ordered TCP preserves earlier references; the queue
        // rejects dependent frames until an IDR, so this need not consume a transport retry.
        if (!queueLossReported) {
            queueLossReported = true
            diagnostic("browser WebCodecs: video-queue-recovery expired=${queue.expiredFrames} overflow=${queue.overflowFrames}")
        }
    }
    private fun resetChain() {
        fail("video-reference-lost", true)
    }
    @Synchronized private fun tick() {
        if (closed || owner == null) return
        val now = clock()
        if (configuringAt?.let { now - it >= TIMEOUT_NS } == true ||
            pending.firstOrNull()?.let { now - it.at >= TIMEOUT_NS } == true) {
            fail("video-progress-timeout", true); return
        }
        if (!ready) return
        if (queue.needsKey) requestKey()
        if (queue.recoveryExpired(now, TIMEOUT_NS) ||
            queue.sourceActive(now) && queueRecoveryAt?.let { now - it >= TIMEOUT_NS } == true) {
            fail("video-keyframe-timeout", true); return
        }
        if (now - reportedAt >= 1_000_000_000L) {
            reportedAt = now
            if (control?.invoke(JSONObject().put("type", "video-progress").put("generation", generation)
                    .put("sent", sequence).put("acknowledged", acknowledged)
                    .put("inFlightFrames", pending.size).put("inFlightBytes", pending.sumOf { it.bytes })
                    .put("expiredFrames", queue.expiredFrames).put("overflowFrames", queue.overflowFrames)
                    .put("keyframeSkips", queue.keyframeSkips)
                    .put("queueBudgetMs", RtcFrameQueue.MAX_AGE_NS / 1_000_000)) != true) {
                fail("video-send-failed", true); return
            }
        }
        if (pending.size >= MAX_IN_FLIGHT) return
        val frame = queue.poll(now)
        noteQueueLoss()
        if (frame == null) return
        if (pending.sumOf { it.bytes.toLong() } + frame.bytes.size > RtcFrameQueue.MAX_BYTES) {
            resetChain(); return
        }
        if (origin == null) origin = frame.nanos
        timestamp = maxOf(timestamp + 1, (frame.nanos - origin!!).coerceAtLeast(0) / 1000)
        val packet = ByteBuffer.allocate(HEADER_BYTES + frame.bytes.size).order(ByteOrder.LITTLE_ENDIAN)
            .putInt(MAGIC).putInt(if (frame.key) 1 else 0)
            .putDouble(generation.toDouble()).putDouble((++sequence).toDouble())
            .putDouble(timestamp.toDouble()).put(frame.bytes).array()
        pending.addLast(Pending(sequence, frame.bytes.size, now))
        if (send?.invoke(packet) != true) { fail("video-send-failed", true); return }
    }
    private fun fail(reason: String, retryable: Boolean) {
        diagnostic("browser WebCodecs: $reason")
        control?.invoke(JSONObject().put("type", "video-error").put("generation", generation)
            .put("reason", reason).put("retryable", retryable))
        detach()
    }
    @Synchronized override fun close() { closed = true; detach(); worker.shutdownNow() }
    companion object {
        const val MAGIC = 0x31435657
        const val HEADER_BYTES = 32
        const val MAX_IN_FLIGHT = 6
        const val TIMEOUT_NS = 3_000_000_000L
        private val GENERATIONS = AtomicLong()
    }
}
