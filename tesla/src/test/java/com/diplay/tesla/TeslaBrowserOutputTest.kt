package com.diplay.tesla

import android.app.Application
import com.diplay.browser.BrowserDisplay
import com.diplay.browser.BrowserMode
import com.diplay.browser.BrowserResolution
import com.shilapi.xcertplay.AppLocale
import com.shilapi.xcertplay.NavigationAudioSettings
import com.shilapi.xcertplay.airplay.AirPlayConfig
import com.shilapi.xcertplay.airplay.AirPlayDisplayConfig
import com.shilapi.xcertplay.airplay.AirPlayInfoPlist
import org.junit.After
import org.junit.Assert.*
import org.junit.Test
import org.junit.runner.RunWith
import org.robolectric.RobolectricTestRunner
import org.robolectric.RuntimeEnvironment
import org.robolectric.annotation.Config

@RunWith(RobolectricTestRunner::class)
@Config(sdk = [29], application = Application::class)
class TeslaBrowserOutputTest {
    private val context get() = RuntimeEnvironment.getApplication()
    @After fun clean() {
        com.shilapi.xcertplay.CarPlayOutput.extension = null
        context.getSharedPreferences("xcertplay_airplay",0).edit().clear().commit()
        context.getSharedPreferences("tesla-browser",0).edit().clear().commit()
        context.getSharedPreferences("diplay",0).edit().clear().commit()
    }
    @Test fun hevcDefaultsOffForTeslaAndRequiresAnExplicitOwnerChoice() {
        com.shilapi.xcertplay.CarPlayOutput.extension = TeslaBrowserOutput(context)
        assertTrue(com.shilapi.xcertplay.CarPlayOutput.extension!!.encodedVideoPassthrough)
        assertFalse(com.shilapi.xcertplay.AirPlayPersistence.loadHevcEnabled(context))
        com.shilapi.xcertplay.AirPlayPersistence.saveHevcEnabled(context, false)
        assertFalse(com.shilapi.xcertplay.AirPlayPersistence.loadHevcEnabled(context))
        com.shilapi.xcertplay.AirPlayPersistence.saveHevcEnabled(context, true)
        assertTrue(com.shilapi.xcertplay.AirPlayPersistence.loadHevcEnabled(context))
    }
    @Test fun remoteCanvasKeepsTheOwnersHevcChoiceWithoutChangingAudioOrIdentity() {
        val output = TeslaBrowserOutput(context)
        val config = AirPlayConfig("DiPlay","device","bt","version", AirPlayDisplayConfig(640,480),
            cluster = AirPlayDisplayConfig(480,320), hevc = true, microphone = true, mainBufferedAudio = true)
        val selected = BrowserDisplay(BrowserMode.FULL,full = BrowserResolution(1600,900),fps = 60)
        output.saveDisplay(selected)
        val actual = output.configure(config)
        assertEquals(1600,actual.main.widthPixels)
        assertEquals(900,actual.main.heightPixels)
        assertEquals(60,actual.main.fps)
        assertEquals(30,output.status().getJSONObject("display").getInt("jpegFps"))
        assertTrue(actual.hevc)
        assertFalse(output.configure(config.copy(hevc = false)).hevc)
        assertNull(actual.cluster)
        assertNull(actual.main.viewAreas)
        assertTrue(actual.microphone)
        assertTrue(actual.mainBufferedAudio)
        assertTrue(actual.externalMainAudio)
        val audio = AirPlayInfoPlist.build(actual)
        assertEquals(9, (audio["audioFormats"] as List<*>).size)
        assertFalse(audio.containsKey("mainBufferedInfo"))
        assertEquals(config.deviceId,actual.deviceId)
        assertEquals(selected,TeslaBrowserOutput(context).status().getJSONObject("display").let(com.diplay.browser.BrowserDisplayJson::decode))
    }
    @Test fun changingProfileDefersNegotiationAndSavingTheSameProfileDoesNotRequestReconnect() {
        val output = TeslaBrowserOutput(context)
        output.configure(AirPlayConfig("DiPlay","device","bt","version",AirPlayDisplayConfig(640,480)))
        assertFalse(output.status().getBoolean("pendingReconnect"))
        output.saveDisplay(BrowserDisplay(mode = BrowserMode.FULL))
        val state = output.status()
        assertTrue(state.getBoolean("pendingReconnect"))
        assertEquals("RIGHT",state.getJSONObject("activeDisplay").getString("mode"))
        assertEquals("FULL",state.getJSONObject("display").getString("mode"))
    }
    @Test fun optedInCarBluetoothOffersNoCarPlayAudioAndReportsTheAppliedProfile() {
        com.shilapi.xcertplay.AirPlayPersistence.saveCarBluetoothAudio(context, true)
        val output = TeslaBrowserOutput(context)
        assertEquals("bluetooth", output.status().getString("audio"))
        val config = AirPlayConfig("DiPlay", "device", "bt", "version", AirPlayDisplayConfig(640, 480),
            disableAudioOutput = true, microphone = true, mainBufferedAudio = true)
        val actual = output.configure(config)
        assertTrue(actual.disableAudioOutput)
        assertFalse(actual.microphone)
        val info = AirPlayInfoPlist.build(actual)
        assertFalse(info.containsKey("audioFormats"))
        assertFalse(info.containsKey("audioLatencies"))
        assertFalse(info.containsKey("mainBufferedInfo"))
        assertEquals("bluetooth", output.status().getString("audio"))
        val restored = output.configure(config.copy(disableAudioOutput = false))
        assertTrue(restored.microphone)
        assertTrue(restored.mainBufferedAudio)
        assertTrue(AirPlayInfoPlist.build(restored).containsKey("audioFormats"))
        assertEquals("android", output.status().getString("audio"))
    }
    @Test fun browserNavigationLevelUsesTheHostSettingAndSurvivesOutputRecreation() {
        NavigationAudioSettings.save(context, -8)
        val first = TeslaBrowserOutput(context)
        assertEquals(-8, first.status().getInt("navigationAttenuationDb"))
        assertTrue(first.saveNavigationLevel(-8, -14))
        val restored = TeslaBrowserOutput(context)
        assertEquals(-14, restored.status().getInt("navigationAttenuationDb"))
        assertFalse(restored.saveNavigationLevel(-8, -5))
        NavigationAudioSettings.save(context, -6)
        assertEquals(-6, first.status().getInt("navigationAttenuationDb"))
        assertFalse(first.status().getBoolean("pendingReconnect"))
    }
    @Test @Config(qualifiers = "fr") fun cnLanguageFallbackRunsOnceAndPreservesTheChosenLanguage() {
        TeslaLocaleDefault.apply(context)
        assertEquals(AppLocale.SIMPLIFIED_CHINESE,AppLocale.preference(context))
        AppLocale.save(context,AppLocale.ENGLISH)
        TeslaLocaleDefault.apply(context)
        assertEquals(AppLocale.ENGLISH,AppLocale.preference(context))
    }
    @Test @Config(qualifiers = "uk") fun supportedUkrainianKeepsSystemLanguage() {
        TeslaLocaleDefault.apply(context)
        assertEquals(AppLocale.SYSTEM,AppLocale.preference(context))
    }
}
