import type { Config } from 'tailwindcss';

export default {
  content: ['./src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      colors: {
        // Named after what they mean on the globe, not what they look like, so
        // the map legend and the CSS cannot drift apart. These match the RGBA
        // values plan-gateway emits in CZML — one distinction that has to
        // survive at a glance is live versus superseded.
        acquisition: { active: '#40c4ff', executed: '#50dc8c', superseded: '#a0a0aa' },
      },
    },
  },
  plugins: [],
} satisfies Config;
