# Dependency security decisions

## Expo Metro `image-size`

The mobile toolchain currently resolves `image-size@1.2.1` through
`expo > @expo/metro > metro > image-size`. pnpm reports two high-severity
advisories for this package, and neither advisory has a patched release.

We temporarily accept this build-time risk rather than force an unsupported
Metro transitive override. Expo SDK 55 controls the Metro version, and replacing
its image parser independently would put the mobile bundler outside Expo's
supported dependency set.

The package processes project assets during mobile bundling; it is not included
in the web, desktop, or server runtime. Mobile source and assets must therefore
remain trusted and reviewed before entering CI. Revisit this decision whenever
Expo or Metro changes its image parser, when `image-size` publishes a fix, and
before the next Expo SDK upgrade.
