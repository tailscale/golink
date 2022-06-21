module.exports = {
  content: ["./tmpl/*.html"],
  theme: {
    colors: {
      blue: {
        50: "rgba(228, 233, 240, 0.15)",
        100: "rgba(220, 233, 242, 0.6)",
        200: "rgba(43, 123, 185, 0.15)",
        300: "#80bdff",
        500: "#4a7ddd",
        600: "#4D78C8",
        700: "#496495",
        800: "#2A4067",
      },
      gray: {
        50: "#faf9f8",
        100: "#f6f4f2",
        200: "#E6E4E2",
        300: "#D6D2CC",
        400: "#B6B0AD",
        500: "#9F9995",
        600: "#666361",
        700: "#474645",
        800: "#343433",
        900: "#242424",
      },
      white: '#fff',
      current: 'currentColor',
    },
    extend: {
      typography: {
        DEFAULT: {
          css: {
            'code::before': {
              'content': '',
            },
            'code::after': {
              'content': '',
            },
          },
        },
      },
    },
  },
  plugins: [
    require('@tailwindcss/forms'),
    require('@tailwindcss/typography'),
  ],
}
