/** @type {import('tailwindcss').Config} */
export default {
  content: ['./index.html', './src/**/*.{ts,tsx}'],
  theme: {
    extend: {
      fontFamily: {
        sans: ['Maple UI', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'monospace'],
        mono: ['Maple UI', 'ui-monospace', 'SFMono-Regular', 'Menlo', 'monospace'],
      },
    },
  },
  plugins: [require('daisyui')],
  daisyui: {
    themes: ['fantasy'],
    darkTheme: false,
  },
}
