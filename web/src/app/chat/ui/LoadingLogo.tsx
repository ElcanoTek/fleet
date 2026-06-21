"use client";

import { useEffect, useState } from "react";

/**
 * LoadingLogo v4 — orbital
 *
 * Petals orbit the center star 360° and lock back in.
 * Star counter-rotates −90°. Particles emit during orbit.
 *
 * CSS is injected into <head> (not SVG defs) for reliable cross-browser animation.
 * A unique prefix per instance prevents style collisions if mounted multiple times.
 *
 * Usage:
 *   <LoadingLogo size={80} />
 *   <LoadingLogo size={48} color="#5a5aaa" />
 */

let instanceCount = 0;

interface LoadingLogoProps {
  size?: number;
  color?: string;
  className?: string;
}

export function LoadingLogo({ size = 80, color = "#7272ab", className }: LoadingLogoProps) {
  // Stable, per-instance prefix so multiple mounted logos don't collide on
  // CSS/SVG ids. Lazy useState runs the initializer exactly once on mount —
  // unlike reading useRef().current during render (which the react-hooks
  // rules flag, and which also re-evaluated `++instanceCount` every render).
  const [prefix] = useState(() => `ll${++instanceCount}`);

  useEffect(() => {
    const p = prefix;
    const style = document.createElement("style");
    style.id = `${p}-styles`;
    style.textContent = `
      #${p}-glow {
        transform-origin: 826.655px 826.715px;
        animation: ${p}-glow 2.2s cubic-bezier(0.4,0,0.6,1) infinite;
      }
      @keyframes ${p}-glow {
        0%   { opacity:.75; transform:scale(0.94); }
        16%  { opacity:0;   transform:scale(1.28); }
        84%  { opacity:0;   transform:scale(1.28); }
        95%  { opacity:.5;  transform:scale(0.91); }
        100% { opacity:.75; transform:scale(0.94); }
      }

      #${p}-star {
        transform-origin: 826.655px 826.715px;
        animation: ${p}-star 2.2s infinite;
      }
      @keyframes ${p}-star {
        0%   { transform:rotate(0deg)   scale(1);    opacity:1;    animation-timing-function:cubic-bezier(0.4,0,0.1,1); }
        16%  { transform:rotate(0deg)   scale(0.86); opacity:.75;  animation-timing-function:linear; }
        84%  { transform:rotate(-90deg) scale(0.86); opacity:.75;  animation-timing-function:cubic-bezier(0.34,1.6,0.64,1); }
        95%  { transform:rotate(-92deg) scale(1.07); opacity:1;    animation-timing-function:cubic-bezier(0.3,0,0.2,1); }
        100% { transform:rotate(-90deg) scale(1);    opacity:1; }
      }

      @keyframes ${p}-orbit {
        0%   { transform:rotate(0deg)     scale(1);     animation-timing-function:cubic-bezier(0.32,0,0.06,1); }
        16%  { transform:rotate(0deg)     scale(1.055); animation-timing-function:linear; }
        84%  { transform:rotate(360deg)   scale(1.055); animation-timing-function:cubic-bezier(0.34,1.55,0.64,1); }
        94%  { transform:rotate(361.5deg) scale(0.975); animation-timing-function:cubic-bezier(0.3,0,0.2,1); }
        100% { transform:rotate(360deg)   scale(1); }
      }

      #${p}-tr { transform-origin:826.655px 826.715px; animation:${p}-orbit 2.2s 0s     infinite; }
      #${p}-tl { transform-origin:826.655px 826.715px; animation:${p}-orbit 2.2s 0.015s infinite; }
      #${p}-bl { transform-origin:826.655px 826.715px; animation:${p}-orbit 2.2s 0.030s infinite; }
      #${p}-br { transform-origin:826.655px 826.715px; animation:${p}-orbit 2.2s 0.010s infinite; }

      .${p}-pt { transform-origin:826.655px 826.715px; }

      @keyframes ${p}-p1 {
        0%,16%  { transform:rotate(-20deg) translate(0,-490px) scale(0);    opacity:0;    animation-timing-function:linear; }
        28%     { transform:rotate(30deg)  translate(0,-545px) scale(1);    opacity:.65;  animation-timing-function:linear; }
        56%     { transform:rotate(90deg)  translate(0,-615px) scale(.5);   opacity:.3;   animation-timing-function:linear; }
        74%,100%{ transform:rotate(140deg) translate(0,-660px) scale(0);    opacity:0; }
      }
      @keyframes ${p}-p2 {
        0%,16%  { transform:rotate(70deg)  translate(0,-455px) scale(0);    opacity:0;    animation-timing-function:linear; }
        33%     { transform:rotate(120deg) translate(0,-520px) scale(1);    opacity:.6;   animation-timing-function:linear; }
        59%     { transform:rotate(175deg) translate(0,-590px) scale(.5);   opacity:.28;  animation-timing-function:linear; }
        77%,100%{ transform:rotate(215deg) translate(0,-635px) scale(0);    opacity:0; }
      }
      @keyframes ${p}-p3 {
        0%,16%  { transform:rotate(160deg) translate(0,-468px) scale(0);    opacity:0;    animation-timing-function:linear; }
        25%     { transform:rotate(200deg) translate(0,-515px) scale(1);    opacity:.68;  animation-timing-function:linear; }
        53%     { transform:rotate(255deg) translate(0,-580px) scale(.55);  opacity:.3;   animation-timing-function:linear; }
        73%,100%{ transform:rotate(292deg) translate(0,-628px) scale(0);    opacity:0; }
      }
      @keyframes ${p}-p4 {
        0%,16%  { transform:rotate(252deg) translate(0,-478px) scale(0);    opacity:0;    animation-timing-function:linear; }
        36%     { transform:rotate(295deg) translate(0,-538px) scale(1);    opacity:.56;  animation-timing-function:linear; }
        63%     { transform:rotate(345deg) translate(0,-608px) scale(.46);  opacity:.24;  animation-timing-function:linear; }
        79%,100%{ transform:rotate(22deg)  translate(0,-648px) scale(0);    opacity:0; }
      }
      @keyframes ${p}-p5 {
        0%,16%  { transform:rotate(30deg)  translate(0,-398px) scale(0);    opacity:0;    animation-timing-function:linear; }
        41%     { transform:rotate(84deg)  translate(0,-458px) scale(.72);  opacity:.46;  animation-timing-function:linear; }
        67%,100%{ transform:rotate(138deg) translate(0,-528px) scale(0);    opacity:0; }
      }
      @keyframes ${p}-p6 {
        0%,16%  { transform:rotate(122deg) translate(0,-418px) scale(0);    opacity:0;    animation-timing-function:linear; }
        43%     { transform:rotate(173deg) translate(0,-480px) scale(.72);  opacity:.46;  animation-timing-function:linear; }
        69%,100%{ transform:rotate(225deg) translate(0,-548px) scale(0);    opacity:0; }
      }
      @keyframes ${p}-p7 {
        0%,16%  { transform:rotate(212deg) translate(0,-408px) scale(0);    opacity:0;    animation-timing-function:linear; }
        39%     { transform:rotate(263deg) translate(0,-465px) scale(.72);  opacity:.46;  animation-timing-function:linear; }
        65%,100%{ transform:rotate(312deg) translate(0,-530px) scale(0);    opacity:0; }
      }
      @keyframes ${p}-p8 {
        0%,16%  { transform:rotate(317deg) translate(0,-428px) scale(0);    opacity:0;    animation-timing-function:linear; }
        45%     { transform:rotate(366deg) translate(0,-495px) scale(.72);  opacity:.46;  animation-timing-function:linear; }
        71%,100%{ transform:rotate(55deg)  translate(0,-562px) scale(0);    opacity:0; }
      }

      #${p}-pt1 { animation:${p}-p1 2.2s 0s     infinite; }
      #${p}-pt2 { animation:${p}-p2 2.2s 0.06s  infinite; }
      #${p}-pt3 { animation:${p}-p3 2.2s 0.03s  infinite; }
      #${p}-pt4 { animation:${p}-p4 2.2s 0.09s  infinite; }
      #${p}-pt5 { animation:${p}-p5 2.2s 0.13s  infinite; }
      #${p}-pt6 { animation:${p}-p6 2.2s 0.16s  infinite; }
      #${p}-pt7 { animation:${p}-p7 2.2s 0.05s  infinite; }
      #${p}-pt8 { animation:${p}-p8 2.2s 0.11s  infinite; }

      @media (prefers-reduced-motion: reduce) {
        #${p}-star, #${p}-tr, #${p}-tl, #${p}-bl, #${p}-br, #${p}-glow,
        #${p}-pt1, #${p}-pt2, #${p}-pt3, #${p}-pt4,
        #${p}-pt5, #${p}-pt6, #${p}-pt7, #${p}-pt8 {
          animation: none !important;
        }
      }
    `;
    document.head.appendChild(style);
    return () => { document.getElementById(`${p}-styles`)?.remove(); };
  }, [prefix]);

  const p = prefix;

  const STAR = `
    M 1135.96 575.54
    C 1138.79 559.75, 1145.41 522.90, 1145.41 522.90
    C 1145.41 522.90, 1107.18 528.79, 1090.80 531.31
    C 937.38 651.67, 719.32 649.81, 568.01 525.54
    C 552.45 522.96, 516.15 516.93, 516.15 516.93
    C 516.15 516.93, 522.21 553.17, 524.81 568.70
    C 648.91 719.78, 650.97 937.40, 531.17 1090.79
    C 528.64 1107.23, 522.74 1145.58, 522.74 1145.58
    C 522.74 1145.58, 559.72 1138.94, 575.57 1136.09
    C 723.95 1019.83, 932.66 1017.86, 1083.05 1130.25
    C 1099.78 1133.05, 1138.81 1139.58, 1138.81 1139.58
    C 1138.81 1139.58, 1132.32 1100.49, 1129.54 1083.73
    C 1016.98 933.10, 1019.15 723.95, 1135.96 575.54 Z
  `;

  return (
    <svg
      xmlns="http://www.w3.org/2000/svg"
      viewBox="-300 -450 2253.31 2553.43"
      width={size}
      height={size}
      className={className}
      aria-label="Loading"
      role="img"
      style={{ display: "block", overflow: "visible" }}
    >
      <defs>
        <radialGradient id={`${p}-grad`} cx="50%" cy="50%" r="50%">
          <stop offset="0%"   stopColor={color} stopOpacity="0.2" />
          <stop offset="100%" stopColor={color} stopOpacity="0"   />
        </radialGradient>
      </defs>

      <ellipse id={`${p}-glow`} cx="826.655" cy="826.715" rx="700" ry="700" fill={`url(#${p}-grad)`} />

      <g id={`${p}-star`}>
        <path fill={color} d={STAR} />
      </g>

      <g id={`${p}-tr`}>
        <path fill={color} d="M 1135.96 575.55 C 1273.07 698.87,1454.41 773.98,1653.31 773.98 V 0 H 879.34 C 879.34 205.80,959.81 392.68,1090.80 531.31 Z" />
      </g>
      <g id={`${p}-tl`}>
        <path fill={color} d="M 568.01 525.54 C 695.78 387.51,773.97 202.93,773.97 0 C 346.52 0,0 346.52,0 773.97 C 202.54 773.97,386.85 696.07,524.81 568.70 Z" />
      </g>
      <g id={`${p}-bl`}>
        <path fill={color} d="M 531.17 1090.79 C 392.54 959.86,205.70 879.45,0 879.45 V 1653.42 H 773.97 C 773.97 1454.50,698.87 1273.18,575.57 1136.09 Z" />
      </g>
      <g id={`${p}-br`}>
        <path fill={color} d="M 1083.05 1130.25 C 956.60 1268.01,879.34 1451.66,879.34 1653.42 C 1306.79 1653.42,1653.31 1306.90,1653.31 879.45 C 1451.28 879.45,1267.38 956.94,1129.54 1083.73 Z" />
      </g>

      {[1,2,3,4].map(i => (
        <g key={i} id={`${p}-pt${i}`} className={`${p}-pt`}>
          <circle cx="826.655" cy="826.715" r="18" fill={color} opacity="0" />
        </g>
      ))}
      {[5,6,7,8].map(i => (
        <g key={i} id={`${p}-pt${i}`} className={`${p}-pt`}>
          <circle cx="826.655" cy="826.715" r="11" fill={color} opacity="0" />
        </g>
      ))}
    </svg>
  );
}

export default LoadingLogo;
