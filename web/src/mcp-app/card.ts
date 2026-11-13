import { mount } from 'svelte';
import ItemCard from './ItemCard.svelte';
import { connectApp } from './bridge';

mount(ItemCard, { target: document.getElementById('app')!, props: { connect: connectApp } });
