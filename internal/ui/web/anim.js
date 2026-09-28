// anim.js - spring/easing animation primitives for the Liquid Glass UI.
// A one-to-one port of the prototype's Anim.cs: Ease, Spring, Tween, Loop and
// ColorLerp. Values ease toward targets; nothing is ever linear.
(function (root) {
  'use strict';

  var Ease = {
    OutCubic: function (t) { t = 1 - t; return 1 - t * t * t; },

    OutBack: function (t) {
      var c1 = 1.70158, c3 = c1 + 1;
      var u = t - 1;
      return 1 + c3 * u * u * u + c1 * u * u;
    },

    InOutCubic: function (t) {
      return t < 0.5 ? 4 * t * t * t : 1 - Math.pow(-2 * t + 2, 3) / 2;
    },

    OutQuint: function (t) { t = 1 - t; return 1 - t * t * t * t * t; },

    Clamp01: function (v) { return v < 0 ? 0 : (v > 1 ? 1 : v); }
  };

  // A float that chases a target with spring-like smoothing.
  function Spring(initial, stiffness, damping) {
    this.Value = initial;
    this.Target = initial;
    this._vel = 0;
    this._stiffness = stiffness === undefined ? 170 : stiffness;
    this._damping = damping === undefined ? 22 : damping;
  }
  Spring.prototype.Set = function (target) { this.Target = target; };
  Spring.prototype.Snap = function (v) { this.Value = this.Target = v; this._vel = 0; };
  // dt in seconds. Returns true while still moving.
  Spring.prototype.Step = function (dt) {
    if (dt > 0.05) dt = 0.05;                  // clamp after a stall
    var f = -this._stiffness * (this.Value - this.Target) - this._damping * this._vel;
    this._vel += f * dt;
    this.Value += this._vel * dt;
    if (Math.abs(this.Value - this.Target) < 0.0008 && Math.abs(this._vel) < 0.0008) {
      this.Value = this.Target; this._vel = 0; return false;
    }
    return true;
  };

  // A one-shot timeline 0..1 with an easing curve.
  function Tween() {
    this.T = 0;            // raw 0..1
    this.Running = false;
    this._dur = 0.016;
    this._ease = Ease.OutCubic;
    this._reverse = false;
  }
  Object.defineProperty(Tween.prototype, 'Eased', {
    get: function () { return this._ease(Ease.Clamp01(this.T)); }
  });
  Tween.prototype.Start = function (seconds, ease, reverse) {
    this._dur = Math.max(0.016, seconds);
    this._ease = ease || Ease.OutCubic;
    this._reverse = !!reverse;
    this.T = this._reverse ? 1 : 0;
    this.Running = true;
  };
  Tween.prototype.Finish = function () { this.T = this._reverse ? 0 : 1; this.Running = false; };
  Tween.prototype.Step = function (dt) {
    if (!this.Running) return false;
    var d = dt / this._dur;
    this.T += this._reverse ? -d : d;
    if ((!this._reverse && this.T >= 1) || (this._reverse && this.T <= 0)) {
      this.T = this._reverse ? 0 : 1;
      this.Running = false;
      return false;
    }
    return true;
  };

  // Free-running phase for looping effects (shimmer, breathing, rotation).
  function Loop(periodSeconds) {
    this.Phase = 0;
    this._period = periodSeconds;
  }
  Loop.prototype.Step = function (dt) {
    this.Phase += dt / this._period;
    if (this.Phase >= 1) this.Phase -= Math.floor(this.Phase);
  };
  Object.defineProperty(Loop.prototype, 'Sin01', {
    get: function () { return 0.5 + 0.5 * Math.sin(this.Phase * Math.PI * 2); }
  });

  // Colours are {a, r, g, b} with 0..255 channels, like System.Drawing.Color.
  var ColorLerp = {
    Mix: function (a, b, t) {
      t = Ease.Clamp01(t);
      return {
        a: Math.trunc(a.a + (b.a - a.a) * t),
        r: Math.trunc(a.r + (b.r - a.r) * t),
        g: Math.trunc(a.g + (b.g - a.g) * t),
        b: Math.trunc(a.b + (b.b - a.b) * t)
      };
    }
  };

  var api = { Ease: Ease, Spring: Spring, Tween: Tween, Loop: Loop, ColorLerp: ColorLerp };
  root.Anim = api;
  if (typeof module === 'object' && module.exports) module.exports = api;
})(typeof self !== 'undefined' ? self : this);
