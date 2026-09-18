// resample_probe.cpp -- standalone offline test of the EXACT resample path
// xenia_plugin.cpp uses (PerChannelResampler with leftover-carry,
// kNativeBlock=64, factor=48000/40000=1.2), against a real xt::Xt instance,
// dumped to a 48kHz WAV -- to find out whether the resample chain itself
// produces glitches, independent of ALSA/Live/hardware entirely.
#include <cstdint>
#include <cstdio>
#include <cstring>
#include <string>
#include <vector>
#include <fstream>
#include <unistd.h>

#include "xtLib/xt.h"
#include "xtLib/xtRomLoader.h"
#include "dsp56kBase/logging.h"
#include "libresample.h"

namespace
{
	constexpr uint32_t kNativeBlock = 64;
	constexpr double kNativeRate = 40000.0;
	constexpr double kHostRate = 48000.0;

	int32_t signExtend24(dsp56k::TWord w)
	{
		struct { int32_t v : 24; } s{ static_cast<int32_t>(w & 0xFFFFFF) };
		return s.v;
	}

	struct PerChannelResampler
	{
		void *handle = nullptr;
		std::vector<float> pending;
		std::vector<float> leftover;
		void open(double factor) { handle = resample_open(1, factor, factor); }
	};

	void writeWav(const std::string &path, const std::vector<int16_t> &interleaved, uint32_t rate)
	{
		std::ofstream f(path, std::ios::binary);
		const uint32_t numSamples = static_cast<uint32_t>(interleaved.size());
		const uint32_t dataBytes = numSamples * 2;
		const uint32_t byteRate = rate * 2 * 2;
		auto w32 = [&](uint32_t v) { f.write(reinterpret_cast<const char *>(&v), 4); };
		auto w16 = [&](uint16_t v) { f.write(reinterpret_cast<const char *>(&v), 2); };
		f.write("RIFF", 4); w32(36 + dataBytes); f.write("WAVE", 4);
		f.write("fmt ", 4); w32(16); w16(1); w16(2); w32(rate); w32(byteRate); w16(4); w16(16);
		f.write("data", 4); w32(dataBytes);
		f.write(reinterpret_cast<const char *>(interleaved.data()), dataBytes);
	}
}

int main(int argc, char **argv)
{
	if(argc < 3)
	{
		fprintf(stderr, "usage: %s <rom_dir> <out.wav>\n", argv[0]);
		return 1;
	}
	if(chdir(argv[1]) != 0)
	{
		fprintf(stderr, "cannot chdir to %s\n", argv[1]);
		return 1;
	}

	Logging::setLogFunc([](const std::string &) {});

	const auto rom = xt::RomLoader::findROM();
	if(!rom.isValid())
	{
		fprintf(stderr, "no valid ROM found in %s\n", argv[1]);
		return 1;
	}

	xt::Xt device(rom.getData(), rom.getFilename());
	if(!device.isValid())
	{
		fprintf(stderr, "device failed to construct from ROM\n");
		return 1;
	}

	fprintf(stderr, "booting...\n");
	while(!device.isBootCompleted())
		device.process(kNativeBlock);
	fprintf(stderr, "boot complete\n");

	const double resampleFactor = kHostRate / kNativeRate;
	PerChannelResampler resampL, resampR;
	resampL.open(resampleFactor);
	resampR.open(resampleFactor);

	auto fillMoreOutput = [&]()
	{
		device.process(kNativeBlock);
		auto &outs = device.getAudioOutputs();

		std::vector<float> nativeL(kNativeBlock), nativeR(kNativeBlock);
		for(uint32_t i = 0; i < kNativeBlock; ++i)
		{
			nativeL[i] = static_cast<float>(signExtend24(outs[0][i])) / 8388608.0f;
			nativeR[i] = static_cast<float>(signExtend24(outs[1][i])) / 8388608.0f;
		}

		auto resampleOne = [&](PerChannelResampler &r, std::vector<float> &in)
		{
			r.leftover.insert(r.leftover.end(), in.begin(), in.end());
			float outBuf[kNativeBlock * 8];
			int inUsed = 0;
			const int outN = resample_process(r.handle, resampleFactor,
				r.leftover.data(), static_cast<int>(r.leftover.size()), 0,
				&inUsed, outBuf, static_cast<int>(std::size(outBuf)));
			if(outN > 0)
				r.pending.insert(r.pending.end(), outBuf, outBuf + outN);
			if(inUsed > 0)
				r.leftover.erase(r.leftover.begin(), r.leftover.begin() + inUsed);
		};
		resampleOne(resampL, nativeL);
		resampleOne(resampR, nativeR);
	};

	// Program 0, note on shortly after, held a while, then off -- same
	// shape as every earlier xenia_render test, just now through the
	// resample chain too.
	auto sendEv = [&](uint8_t status, uint8_t d1, uint8_t d2)
	{
		synthLib::SMidiEvent ev(synthLib::MidiEventSource::Host, status, d1, d2);
		device.sendMidiEvent(ev);
	};

	sendEv(0xC0 | 9, 0, 0); // program change 0, channel 10 (0-indexed 9)
	for(int i = 0; i < 200; ++i) device.process(kNativeBlock);

	sendEv(0x90 | 9, 60, 100); // note on 60, vel 100
	for(int i = 0; i < 50; ++i) device.process(kNativeBlock);

	const uint32_t totalOutFrames = static_cast<uint32_t>(kHostRate * 4); // 4 seconds
	std::vector<int16_t> interleaved;
	interleaved.reserve(totalOutFrames * 2);

	uint32_t noteOffAt = totalOutFrames * 3 / 4;
	bool noteOffSent = false;

	for(uint32_t f = 0; f < totalOutFrames; ++f)
	{
		if(!noteOffSent && f >= noteOffAt)
		{
			sendEv(0x80 | 9, 60, 0);
			noteOffSent = true;
		}
		while(resampL.pending.size() < 1) fillMoreOutput();
		while(resampR.pending.size() < 1) fillMoreOutput();

		auto clamp16 = [](float v) -> int16_t
		{
			const float scaled = v * 0.8f * 32767.0f;
			if(scaled > 32767.0f) return 32767;
			if(scaled < -32768.0f) return -32768;
			return static_cast<int16_t>(scaled);
		};
		interleaved.push_back(clamp16(resampL.pending.front()));
		interleaved.push_back(clamp16(resampR.pending.front()));
		resampL.pending.erase(resampL.pending.begin());
		resampR.pending.erase(resampR.pending.begin());
	}

	writeWav(argv[2], interleaved, static_cast<uint32_t>(kHostRate));
	fprintf(stderr, "wrote %s (%u frames at %u Hz)\n", argv[2], totalOutFrames, static_cast<uint32_t>(kHostRate));
	return 0;
}
