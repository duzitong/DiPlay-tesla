package com.diplay.browser.rtc

import com.shilapi.xcertplay.airplay.VideoCodec
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config
import java.nio.ByteBuffer
import java.nio.ByteOrder
import java.util.concurrent.LinkedBlockingQueue
import java.util.concurrent.TimeUnit
import java.util.concurrent.atomic.AtomicInteger
import java.util.concurrent.atomic.AtomicLong

@RunWith(RobolectricTestRunner::class) @Config(sdk = [29])
class H264VideoOutputTest {
    private class Fixture : AutoCloseable {
        val clock = AtomicLong()
        val messages = LinkedBlockingQueue<JSONObject>()
        val packets = LinkedBlockingQueue<ByteArray>()
        val diagnostics = LinkedBlockingQueue<String>()
        val keys = AtomicInteger()
        val stops = AtomicInteger()
        val owner = Any()
        val output = H264VideoOutput({ keys.incrementAndGet() }, { diagnostics.offer(it) }, clock::get)
        fun attach(): Long {
            output.attach(owner, { messages.offer(it); true }, { packets.offer(it); true }, { stops.incrementAndGet() })
            return message("video-config").getLong("generation")
        }
        fun message(type: String): JSONObject {
            val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(2)
            while (System.nanoTime() < deadline) {
                val next = messages.poll(2, TimeUnit.SECONDS) ?: break
                if (next.getString("type") == type) return synchronized(output) { next }
            }
            error("Missing $type")
        }
        fun ready(generation: Long) = output.command(owner, JSONObject().put("type", "video-ready").put("generation", generation))
        fun frame(key: Boolean = true) = output.frame(byteArrayOf(0, 0, 0, 1, if (key) 0x65 else 0x41, 1), clock.get())
        fun packet() = ByteBuffer.wrap(packets.poll(2, TimeUnit.SECONDS)!!).order(ByteOrder.LITTLE_ENDIAN)
        override fun close() = output.close()
    }
    @Test fun configurationPrecedesKeyframeAndPayloadsAreOwnedWithMonotonicMicroseconds() {
        Fixture().use { f ->
            f.output.config(CONFIG)
            val generation = f.attach()
            f.frame(); assertNull(f.packets.poll())
            f.ready(generation); f.frame(false); assertNull(f.packets.poll())
            val input = byteArrayOf(0, 0, 0, 2, 0x65, 0x20)
            f.output.frame(input, 10_000_000); input.fill(0)
            val packet = f.packet()
            assertEquals(H264VideoOutput.MAGIC, packet.int); assertEquals(1, packet.int)
            assertEquals(generation.toDouble(), packet.double, 0.0)
            assertEquals(1.0, packet.double, 0.0); assertEquals(0.0, packet.double, 0.0)
            val payload = ByteArray(packet.remaining()).also { packet.get(it) }
            assertArrayEquals(byteArrayOf(0, 0, 0, 1, 0x67, 0x42, 0, 0x1f,
                0, 0, 0, 1, 0x68, 0, 0, 0, 0, 1, 0x65, 0x20), payload)
            f.output.frame(byteArrayOf(0, 0, 0, 1, 0x41, 1), 10_000_000)
            val next = f.packet()
            next.position(24); assertEquals(1.0, next.double, 0.0)
        }
    }
    @Test fun cumulativeAcksBoundInFlightAndOnlyPendingWorkTimesOut() {
        Fixture().use { f ->
            f.output.config(CONFIG); val generation = f.attach(); f.ready(generation)
            f.clock.set(20_000_000_000)
            Thread.sleep(30); assertEquals(0, f.stops.get())
            repeat(6) { f.frame(it == 0); f.packet() }
            f.output.command(Any(), JSONObject().put("type", "video-ack").put("generation", generation).put("sequence", 6))
            f.output.command(f.owner, JSONObject().put("type", "video-ack").put("generation", generation - 1).put("sequence", 6))
            f.frame(false); assertNull(f.packets.poll(30, TimeUnit.MILLISECONDS))
            f.output.command(f.owner, JSONObject().put("type", "video-ack").put("generation", generation).put("sequence", 6))
            f.packet()
            f.clock.addAndGet(H264VideoOutput.TIMEOUT_NS)
            assertEquals("video-progress-timeout", f.message("video-error").getString("reason"))
            assertEquals(1, f.stops.get())
        }
    }
    @Test fun oversizedFrameAndHevcRejectExplicitlyAndNewLinkRequiresNewConfigAndKey() {
        Fixture().use { f ->
            f.output.config(CONFIG); val old = f.attach(); f.ready(old)
            f.output.frame(ByteArray(RtcFrameQueue.MAX_BYTES + 1).apply { this[3] = 1; this[4] = 0x65 }, 0)
            assertEquals("video-reference-lost", f.message("video-error").getString("reason"))
            val next = f.attach(); assertNotEquals(old, next)
            f.ready(old); f.frame(); assertNull(f.packets.poll())
            f.ready(next); f.frame(); f.packet()
            f.output.codec(VideoCodec.H265)
            val error = f.message("video-error")
            assertEquals("h264-required", error.getString("reason")); assertFalse(error.getBoolean("retryable"))
        }
    }
    @Test fun unsentExpiryAndOverflowWaitForAnIdrWithoutDisconnectingOrLosingDisplayedState() {
        for (expire in listOf(false, true)) Fixture().use { f ->
            f.output.config(CONFIG); val generation = f.attach(); f.ready(generation)
            repeat(H264VideoOutput.MAX_IN_FLIGHT) { f.frame(it == 0); f.packet() }
            val requested = f.keys.get()
            f.frame(false)
            if (expire) f.clock.set(RtcFrameQueue.MAX_AGE_NS)
            else repeat(RtcFrameQueue.MAX_FRAMES) { f.frame(false) }
            f.output.command(f.owner, JSONObject().put("type", "video-ack")
                .put("generation", generation).put("sequence", H264VideoOutput.MAX_IN_FLIGHT))
            val deadline = System.nanoTime() + TimeUnit.SECONDS.toNanos(2)
            var recovery: String? = null
            while (System.nanoTime() < deadline) {
                val next = f.diagnostics.poll(2, TimeUnit.SECONDS) ?: break
                if (next.contains("video-queue-recovery")) { recovery = next; break }
            }
            assertNotNull("Unsent loss must be reported", recovery)
            assertEquals(0, f.stops.get())
            assertTrue(f.output.displayed)
            f.frame(false)
            assertNull("A dependent frame must not cross the gap", f.packets.poll(40, TimeUnit.MILLISECONDS))
            assertTrue(f.keys.get() > requested)
            f.frame()
            val resumed = f.packet()
            assertEquals(H264VideoOutput.MAGIC, resumed.int)
            assertEquals(1, resumed.int)
            assertEquals(generation.toDouble(), resumed.double, 0.0)
            assertEquals((H264VideoOutput.MAX_IN_FLIGHT + 1).toDouble(), resumed.double, 0.0)
            f.output.command(f.owner, JSONObject().put("type", "video-ack")
                .put("generation", generation).put("sequence", H264VideoOutput.MAX_IN_FLIGHT + 1))
            f.clock.addAndGet(H264VideoOutput.TIMEOUT_NS)
            Thread.sleep(40)
            assertEquals(0, f.stops.get())
            assertTrue(f.output.displayed)
            assertTrue(f.messages.none { it.getString("type") in listOf("video-error", "video-config") })
        }
    }
    @Test fun missedInitialKeyframeIsRequestedAgainEvenWhenTheSourceBecomesStill() {
        Fixture().use { f ->
            f.output.config(CONFIG); val generation = f.attach(); f.ready(generation)
            val initialRequests = f.keys.get()
            Thread.sleep(40)
            assertTrue(f.keys.get() > initialRequests)
            f.frame(); f.packet()
            val receivedRequests = f.keys.get()
            Thread.sleep(40)
            assertEquals(receivedRequests, f.keys.get())
        }
    }
    @Test fun localQueueRecoveryStillTimesOutWhenAnActiveSourceNeverSendsAnotherKeyframe() {
        Fixture().use { f ->
            f.output.config(CONFIG); val generation = f.attach(); f.ready(generation)
            f.frame(); f.packet()
            f.output.command(f.owner, JSONObject().put("type", "video-ack")
                .put("generation", generation).put("sequence", 1))
            f.frame(false)
            f.clock.set(RtcFrameQueue.MAX_AGE_NS); f.frame(false)
            repeat(6) { f.clock.addAndGet(500_000_000); f.frame(false) }
            assertEquals("video-keyframe-timeout", f.message("video-error").getString("reason"))
            assertEquals(1, f.stops.get())
        }
    }
    @Test fun repeatedlyQueuedButExpiredKeyframesCannotExtendTheLocalRecoveryDeadline() {
        Fixture().use { f ->
            f.output.config(CONFIG); val generation = f.attach(); f.ready(generation)
            f.frame(); f.packet()
            f.output.command(f.owner, JSONObject().put("type", "video-ack")
                .put("generation", generation).put("sequence", 1))
            synchronized(f.output) {
                f.frame(false); f.clock.set(RtcFrameQueue.MAX_AGE_NS); f.frame(false)
                repeat(6) {
                    f.clock.addAndGet(500_000_000); f.frame()
                    f.clock.addAndGet(RtcFrameQueue.MAX_AGE_NS); f.frame(false)
                }
            }
            assertEquals("video-keyframe-timeout", f.message("video-error").getString("reason"))
            assertEquals(1, f.stops.get())
        }
    }
    @Test fun aStillSourceDoesNotSpendQueueRecoveryTimeAndResumesWithoutATransportReset() {
        Fixture().use { f ->
            f.output.config(CONFIG); val generation = f.attach(); f.ready(generation)
            f.frame(); f.packet()
            f.output.command(f.owner, JSONObject().put("type", "video-ack")
                .put("generation", generation).put("sequence", 1))
            f.frame(false)
            f.clock.set(RtcFrameQueue.MAX_AGE_NS); f.frame(false)
            f.clock.set(60_000_000_000)
            Thread.sleep(40)
            assertEquals(0, f.stops.get())
            assertTrue(f.output.displayed)
            f.frame(false)
            Thread.sleep(40)
            assertEquals("A resumed source gets a fresh recovery budget", 0, f.stops.get())
            f.frame()
            val packet = f.packet()
            packet.position(8)
            assertEquals(generation.toDouble(), packet.double, 0.0)
            assertEquals(2.0, packet.double, 0.0)
        }
    }
    companion object {
        private val CONFIG = byteArrayOf(1,0x42,0,0x1f,-1,-31,0,4,0x67,0x42,0,0x1f,1,0,2,0x68,0)
    }
}
