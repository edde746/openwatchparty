<script setup lang="ts">
// The home page's hero picture: three different players (a browser, a TV, a phone) show the same frame of the same
// title at the same moment, joined to one room code. The frame is a still from the open movie Tears of Steel (CC BY 3.0,
// credited under the picture), in one <symbol> used three times. It moves only for readers who haven't asked for
// reduced motion.
interface Screen {
  x: number
  y: number
  w: number
  h: number
}

// The video area of each player, 16:9.
const screens: Screen[] = [
  { x: 14, y: 38, w: 276, h: 155 },
  { x: 331, y: 49, w: 256, h: 144 },
  { x: 196, y: 313, w: 208, h: 117 },
]

// The playback controls over a video area, scaled to its height.
function controls({ x, y, w, h }: Screen) {
  const line = y + h - h * 0.11
  const track = { x: x + h * 0.2, w: w - h * 0.2 - h * 0.62, y: line - h * 0.014, h: h * 0.028 }
  return {
    shade: { x, y: y + h * 0.62, width: w, height: h * 0.38 },
    pause: [x + h * 0.07, x + h * 0.115].map((bx) => ({ x: bx, y: line - h * 0.04, width: h * 0.03, height: h * 0.08 })),
    track: { x: track.x, y: track.y, width: track.w, height: track.h, rx: track.h / 2 },
    played: { x: track.x, y: track.y, width: track.w * 0.42, height: track.h, rx: track.h / 2 },
    time: { x: x + w - h * 0.07, y: line + h * 0.026, 'font-size': h * 0.075 },
  }
}
</script>

<template>
  <div class="art">
    <svg
      viewBox="0 0 600 450"
      role="img"
      aria-label="A browser, a TV and a phone show the same frame of the same title, joined to room K7M-Q2X"
    >
      <defs>
        <linearGradient id="owp-art-shade" x1="0" y1="0" x2="0" y2="1">
          <stop offset="0" stop-color="#000" stop-opacity="0" />
          <stop offset="1" stop-color="#000" stop-opacity="0.65" />
        </linearGradient>
        <!-- One frame of the title every player shows. -->
        <symbol id="owp-art-frame" viewBox="0 0 160 90" preserveAspectRatio="xMidYMid slice">
          <image href="/tears-of-steel.webp" width="160" height="90" preserveAspectRatio="xMidYMid slice" />
        </symbol>
      </defs>

      <g class="links" fill="none" stroke-width="2" stroke-linecap="round" stroke-dasharray="2 7">
        <path d="M244 234C228 222 206 218 196 208" />
        <path d="M356 234C372 222 390 214 404 204" />
        <path d="M300 280V302" />
      </g>

      <!-- A browser window. -->
      <g class="device">
        <rect class="window" x="4" y="8" width="296" height="204" rx="12" />
        <circle class="chrome" cx="22" cy="23" r="4.5" />
        <circle class="chrome" cx="36" cy="23" r="4.5" />
        <circle class="chrome" cx="50" cy="23" r="4.5" />
        <rect class="chrome" x="68" y="16" width="150" height="14" rx="7" />
      </g>
      <!-- A TV. -->
      <g class="device">
        <rect class="stand" x="449" y="202" width="22" height="14" />
        <rect class="stand" x="414" y="215" width="92" height="7" rx="3.5" />
        <rect class="bezel" x="322" y="40" width="274" height="162" rx="10" />
      </g>
      <!-- A phone on its side. -->
      <g class="device">
        <rect class="bezel" x="172" y="302" width="256" height="140" rx="26" />
        <circle class="lens" cx="184" cy="372" r="3" />
      </g>

      <g v-for="(screen, i) in screens" :key="i">
        <use href="#owp-art-frame" :x="screen.x" :y="screen.y" :width="screen.w" :height="screen.h" />
        <rect v-bind="controls(screen).shade" fill="url(#owp-art-shade)" />
        <rect v-for="(bar, j) in controls(screen).pause" :key="j" v-bind="bar" fill="#fff" />
        <rect v-bind="controls(screen).track" fill="#fff" fill-opacity="0.35" />
        <rect class="played" v-bind="controls(screen).played" fill="#fff" />
        <text class="time" v-bind="controls(screen).time" text-anchor="end" fill="#fff">1:02:14</text>
      </g>

      <!-- The room code. -->
      <g class="code">
        <rect class="pulse" x="210" y="232" width="180" height="48" rx="24" />
        <rect class="chip" x="210" y="232" width="180" height="48" rx="24" />
        <rect x="224" y="242" width="28" height="28" rx="7" fill="#3451b2" />
        <path d="M230.5 249v14l9.6-7z" fill="#fff" fill-opacity="0.5" />
        <path d="M235.75 249v14l9.6-7z" fill="#fff" />
        <text class="code-text" x="318" y="263" text-anchor="middle">K7M-Q2X</text>
      </g>
    </svg>
    <p class="credit">
      Frame from <a href="https://mango.blender.org/">Tears of Steel</a>, (CC) Blender Foundation,
      <a href="https://creativecommons.org/licenses/by/3.0/">CC BY 3.0</a>
    </p>
  </div>
</template>

<style scoped>
.art {
  position: relative;
  margin: 0 auto;
  width: 100%;
  max-width: 600px;
}

/* A soft light behind the players. */
.art::before {
  content: '';
  position: absolute;
  inset: 4% 8%;
  border-radius: 50%;
  background: radial-gradient(closest-side, rgba(100, 108, 255, 0.22), rgba(100, 108, 255, 0));
  filter: blur(24px);
}

svg {
  position: relative;
  display: block;
  width: 100%;
  height: auto;
  overflow: visible;
}

.credit {
  position: relative;
  margin: 8px 0 0;
  text-align: center;
  font-size: 12px;
  line-height: 16px;
  color: var(--vp-c-text-3);
}

.credit a {
  color: inherit;
  text-decoration: underline;
  white-space: nowrap;
}

.device {
  filter: drop-shadow(0 10px 22px rgba(15, 17, 40, 0.16));
}

.dark .device {
  filter: drop-shadow(0 12px 26px rgba(0, 0, 0, 0.5));
}

.window {
  fill: var(--vp-c-bg-elv);
  stroke: var(--vp-c-divider);
}

.chrome {
  fill: var(--vp-c-default-soft);
}

.bezel {
  fill: #121318;
  stroke: #2c2d36;
}

.stand {
  fill: #8d8e99;
}

.dark .stand {
  fill: #3b3c46;
}

.lens {
  fill: #2c2d36;
}

.links {
  stroke: var(--vp-c-brand-1);
}

.time {
  font-family: var(--vp-font-family-mono);
  font-weight: 600;
}

.chip {
  fill: var(--vp-c-bg-elv);
  stroke: var(--vp-c-brand-1);
  stroke-width: 2;
}

.pulse {
  fill: none;
  stroke: var(--vp-c-brand-1);
  stroke-width: 2;
  opacity: 0;
  transform-box: fill-box;
  transform-origin: center;
}

.code-text {
  fill: var(--vp-c-text-1);
  font-family: var(--vp-font-family-mono);
  font-size: 21px;
  font-weight: 700;
  letter-spacing: 0.06em;
}

.played {
  transform-box: fill-box;
  transform-origin: left center;
}

@media (prefers-reduced-motion: no-preference) {
  .links {
    animation: flow 1.2s linear infinite;
  }

  .pulse {
    animation: pulse 3s ease-out infinite;
  }

  /* Every player's progress moves together. */
  .played {
    animation: play 40s linear infinite;
  }
}

@keyframes flow {
  to {
    stroke-dashoffset: -9;
  }
}

@keyframes pulse {
  0% {
    opacity: 0.6;
    transform: scale(1);
  }
  70%,
  100% {
    opacity: 0;
    transform: scale(1.22, 1.6);
  }
}

@keyframes play {
  to {
    transform: scaleX(1.5);
  }
}
</style>
