---
titulo: Farmacia hospitalaria
estado: vigente
tags:
  - hospital
  - servicio-central
  - farmacia
  - medicamentos
descripcion: Validación, elaboración y dispensación de medicamentos; dosis unitaria, citostáticos y pacientes externos.
tipo: contexto
version: "1.0"
autor: Documentación de ejemplo
fecha_creacion: 2026-08-10
derechos: ejemplo-libre
---

# Farmacia hospitalaria

El servicio de farmacia hospitalaria selecciona, adquiere, custodia, prepara y dispensa los
medicamentos del hospital, y valida que cada prescripción sea correcta para ese paciente
concreto. No es un almacén: es un servicio clínico.

## Objetivo

Describir las funciones del servicio de farmacia y su papel como barrera de seguridad en el
circuito del medicamento.

## Alcance

Aplica al medicamento de uso hospitalario, en ingreso, en día y en dispensación ambulatoria.
Queda fuera la receta electrónica de atención primaria.

## Desarrollo

### Validación farmacéutica

Revisión de cada prescripción: dosis según peso y función renal, interacciones, duplicidades
y alergias. Es una barrera de seguridad antes de que el fármaco llegue a la cama.

### Dispensación en dosis unitaria

Preparación de un cajetín por paciente y por día, con cada toma identificada, para las
unidades de [[Hospitalización|hospitalización]].

### Elaboración y farmacotecnia

Preparación de mezclas intravenosas, nutrición parenteral y citostáticos en cabina de
seguridad biológica, con la dosis calculada para el paciente que va a tratarse ese día en
[[Hospital de día|hospital de día]].

### Pacientes externos

Dispensación ambulatoria de medicamentos de uso hospitalario que el paciente se lleva a
casa, con seguimiento de adherencia.

### Gestión del medicamento

Guía farmacoterapéutica del centro, compras, control de estupefacientes, caducidades y
gestión de desabastecimientos.

### Indicadores habituales

Intervenciones farmacéuticas y su aceptación, errores de medicación detectados, consumo y
gasto por servicio, y roturas de stock.

## Documentos relacionados

- [[Cuidados intensivos]]
- [[Bloque quirúrgico]]
- [[Sistemas de información]]

## Observaciones

> [!info] Medicamentos de alto riesgo
> Un grupo reducido de fármacos concentra la mayor parte del daño evitable. Su circuito suele
> tener controles añadidos: doble comprobación, presentaciones diferenciadas y almacenamiento
> separado.

## Control de cambios

| Versión | Fecha | Descripción del cambio | Responsable |
|---|---|---|---|
| 1.0 | 2026-08-10 | Creación inicial | Documentación de ejemplo |