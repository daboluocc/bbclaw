#!/usr/bin/env python3
"""语音链路耗时抓取:跑它,然后按 PTT 问一句、松手、等答案。
VOICE-LAT 各段耗时会实时打出来。默认抓 90s,可传秒数覆盖。用法:
    python3 firmware/scripts/voice_latency.py [秒数] [--port /dev/cu.xxx]

串口自动识别:优先 bench 板 /dev/cu.usbserial-*,否则手表 /dev/cu.usbmodem*。
各段含义:
    ptt_release   松手,t0 开始计时
    transcript    云端 ASR 完成(total=上传+云端 ASR)
    first_delta   LLM 首字(gap=纯 LLM 思考耗时)
    tts_first_pcm 首句出声(total=用户感知「多久出声」,最关键)
"""
import glob, serial, sys, time

def pick_port():
    for pat in ("/dev/cu.usbserial-*", "/dev/cu.usbmodem*"):
        hits = sorted(glob.glob(pat))
        if hits:
            return hits[0]
    return None

args = [a for a in sys.argv[1:] if not a.startswith("--")]
port = None
if "--port" in sys.argv:
    port = sys.argv[sys.argv.index("--port") + 1]
port = port or pick_port()
if not port:
    sys.exit("找不到串口:插上板子,或用 --port 指定")
DUR = int(args[0]) if args else 90

s = serial.Serial(port, 115200, timeout=0.4)
print(f">>> 口={port}  现在按住 PTT 问一句 → 松手 → 等答案播出来。{DUR}s 后自动结束 <<<\n")
t0 = time.time()
lat = []
while time.time() - t0 < DUR:
    raw = s.readline().decode("utf-8", "replace").strip()
    if not raw:
        continue
    if "VOICE-LAT" in raw:
        seg = raw[raw.find("VOICE-LAT"):]
        print("  " + seg)
        lat.append(seg)
    elif any(k in raw for k in ("phase=finish", "cloud_wait mono", "no speech",
                                "empty transcript", "DIZZY")):
        print("  · " + raw[-90:])

print("\n===== 小结 =====")
if lat:
    for x in lat:
        print("  " + x)
    print("\n看 gap 最大的那步,就是最慢的一段(transcript=ASR / first_delta=LLM / tts_first_pcm=TTS)。")
else:
    print("  没抓到 VOICE-LAT。确认这一轮真的按了 PTT 并说了话,且板子连着云端(net=CLOUD)。")
