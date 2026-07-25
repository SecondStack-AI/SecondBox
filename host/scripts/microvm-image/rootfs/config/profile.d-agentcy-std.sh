# Headless matplotlib defaults for the tool-VM sandbox. Agg is auto-selected when
# no display is present, but set it explicitly so scripts that probe MPLBACKEND
# see a stable value. The font cache is pre-warmed at image-build time in the
# default HOME cache location, so the first plot does not pay a font-scan delay.
export MPLBACKEND=Agg
