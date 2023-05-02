module.exports = {
  content: ["./tmpl/*.html"],
  theme: {
    colors: {
      blue: {
        0: "rgba(240, 245, 255)",
        50: "rgba(206, 222, 253)",
        100: "rgba(173, 199, 252)",
        200: "rgba(133, 170, 245)",
        300: "rgba(108, 148, 236)",
        400: "rgba(90, 130, 222)",
        500: "rgba(75, 112, 204)",
        600: "rgba(63, 93, 179)",
        700: "rgba(50, 73, 148)",
        800: "rgba(37, 53, 112)",
        900: "rgba(25, 34, 74)",
      },
      gray: {
        0: "rgba(250, 249, 248)",
        50: "rgba(249, 247, 246)",
        100: "rgba(247, 245, 244)",
        200: "rgba(238, 235, 234)",
        300: "rgba(218, 214, 213)",
        400: "rgba(175, 172, 171)",
        500: "rgba(112, 110, 109)",
        600: "rgba(68, 67, 66)",
        700: "rgba(46, 45, 45)",
        800: "rgba(35, 34, 34)",
        900: "rgba(31, 30, 30)",
      },
      red: {
        0: "rgba(255, 246, 244)",
        50: "rgba(255, 211, 207)",
        100: "rgba(255, 177, 171)",
        200: "rgba(246, 143, 135)",
        300: "rgba(228, 108, 99)",
        400: "rgba(208, 72, 65)",
        500: "rgba(178, 45, 48)",
        600: "rgba(148, 8, 33)",
        700: "rgba(118, 0, 18)",
        800: "rgba(90, 0, 0)",
        900: "rgba(66, 0, 0)",
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
