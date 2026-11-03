package com.diplay.browser

import org.json.JSONArray
import org.json.JSONObject
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class) @Config(sdk = [29])
class BrowserClientDiagnosticsTest {
    private fun body(data: JSONObject = JSONObject().put("reason", "page-hidden")) = JSONObject()
        .put("client", "1234abcd").put("dropped", 0).put("events", JSONArray().put(
            JSONObject().put("seq", 1).put("atMs", 1000).put("elapsedMs", 10)
                .put("event", "media-close").put("data", data)))

    @Test fun onlyBoundedStateAndCountersAreAcceptedAndLongSnapshotsAreNotTruncated() {
        val data = JSONObject()
        for (key in listOf("visibility", "renderer", "transport", "reason", "connection", "ice", "fallbackReason",
            "contextState", "engine", "browser", "errorName")) data.put(key, "a".repeat(48))
        val lines = BrowserClientDiagnostics.lines(body(data).put("dropped", 2))
        assertTrue(lines.first().contains("dropped=2"))
        assertTrue(lines.size > 2)
        assertTrue(lines.all { it.length < 700 })
        assertTrue(lines.joinToString().contains("errorName"))
        for (invalid in listOf(JSONObject().put("url", "https://private"),
            JSONObject().put("reason", "https://private"), JSONObject().put("queuedMs", "private data"))) {
            assertThrows(IllegalArgumentException::class.java) { BrowserClientDiagnostics.lines(body(invalid)) }
        }
    }

    @Test fun oversizedBatchesAndInvalidSequencesAreRejectedBeforeAnyWrite() {
        val data = body()
        val event = data.getJSONArray("events").getJSONObject(0)
        val events = JSONArray()
        repeat(9) { events.put(event) }
        assertThrows(IllegalArgumentException::class.java) { BrowserClientDiagnostics.lines(data.put("events", events)) }
        for (number in listOf(-1, 1.5, "1")) {
            val invalid = body()
            invalid.getJSONArray("events").getJSONObject(0).put("seq", number)
            assertThrows(IllegalArgumentException::class.java) { BrowserClientDiagnostics.lines(invalid) }
        }
    }
    @Test fun savedAudioSnapshotRetainsIndependentVolumesAndGainWithoutPrivatePayloads() {
        val data = JSONObject().put("navigationVolume", 33).put("navigationGain", 0.99)
            .put("musicVolume", 100).put("musicGain", 1)
        val lines = BrowserClientDiagnostics.lines(body(data))
        val saved = JSONObject(lines.single().substringAfter(" data="))
        assertEquals(0.99, saved.getDouble("navigationGain"), 0.0)
        assertEquals(33, saved.getInt("navigationVolume"))
        assertEquals(1, saved.getInt("musicGain"))
        assertEquals(100, saved.getInt("musicVolume"))
        assertThrows(IllegalArgumentException::class.java) {
            BrowserClientDiagnostics.lines(body(data.put("pcm", "private")))
        }
    }
}
