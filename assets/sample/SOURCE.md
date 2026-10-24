# Sample lecture: source and licence

**Licence: Public Domain** (see `LICENSE` in this directory).

| | |
|---|---|
| Work | *The Chemical History of a Candle*, Lecture I ("A Candle: The Flame — Its Sources — Structure — Mobility — Brightness"), opening two paragraphs |
| Author | Michael Faraday (1791–1867); Christmas Lectures at the Royal Institution, 1848 (published 1861, ed. William Crookes) |
| Reader | Availle (LibriVox volunteer, reader id 4174) |
| Recording | LibriVox, "Chemical History of A Candle", section 1 "Lecture I", catalogued 2011-04-16 |
| Catalogue page | https://librivox.org/the-chemical-history-of-a-candle-by-michael-faraday/ |
| Archive item | https://archive.org/details/chemical_history_candle_ava_librivox |
| Downloaded file | https://www.archive.org/download/chemical_history_candle_ava_librivox/chemicalhistorycandle_1_faraday_64kb.mp3 (17,561,226 bytes, MD5 `d08759ac0134272a0bc41865696ceb49`, matches the archive's file list) |
| Reference text | Project Gutenberg eBook #14474, https://www.gutenberg.org/ebooks/14474 (Lecture I, first two paragraphs, verbatim) |

## Licence basis

- The text: Faraday died in 1867 and the lectures were published in 1861; the work is in the public domain worldwide.
- The recording: LibriVox releases all its recordings into the public domain
  (https://librivox.org/pages/public-domain/). The archive.org item carries the
  Creative Commons Public Domain Mark 1.0
  (`licenseurl` = http://creativecommons.org/publicdomain/mark/1.0/).
- The reference text is the public-domain book text; no Project Gutenberg
  header, footer or trademark is included.

## Excerpt

- Start 00:00:29.500, end 00:01:35.000 of the downloaded MP3 (65.5 s). Both cuts
  fall in pauses: the clip starts after the spoken chapter heading, at
  "I purpose, in return for the honour…", and ends after
  "…which could not be better, were it even so good." (end of the second
  paragraph), before "And before proceeding…".
- Only this excerpt is in the repository; the full recording and the book are not.

## Processing

```
ffmpeg -i chemicalhistorycandle_1_faraday_64kb.mp3 -ss 29.5 -to 95.0 -map_metadata -1 \
       -ac 1 -ar 16000 -sample_fmt s16 -c:a flac -compression_level 12 sample-lecture.flac
```

(ffmpeg 9.0.1, gyan.dev full build.) Result: FLAC, 16 kHz, mono, 16-bit, 65.50 s, 1,263,951 bytes,
SHA-256 `e91ed46b8ac723f4ce520f2f2eaa9b025868adf078dc90c1bde0edcf02659d50`.

## Reference text check

`reference.txt` is the book text (UTF-8, one line). The reader follows the book word
for word: a cross-check transcription of `sample-lecture.flac` with faster-whisper
1.2.1, `large-v2`, CUDA float16, language `en`, gives 174 of 174 words and
WER 0.0 under the self-test normalisation (`internal/devices/wer.go`: lower case,
apostrophes removed, every non-`[0-9a-z]` character a space).
